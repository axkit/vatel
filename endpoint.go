package vatel

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"iter"
	"path"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/axkit/errors"
	realip "github.com/axkit/fasthttp-realip"
	"github.com/axkit/vatel/jsonmask"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

// LogOption is a bitmask controlling per-endpoint log verbosity.
type LogOption uint32

const (
	LogSilent     LogOption = 1 << iota // log nothing but 5xx errors; overrides all other bits
	LogEnter                            // log when a request arrives
	LogExit                             // log when a request completes
	LogReqBody                          // log the request body as compacted JSON (masked by `mask` tags)
	LogReqInput                         // log the decoded input struct (masked by `mask` tags)
	LogRespBody                         // log the response body (masked by `mask` tags) under "respBody"
	LogRespOutput                       // log the response body under "result" when LogRespBody is not set
)

var logOptionString = map[string]LogOption{
	"silent":    LogSilent,
	"enter":     LogEnter,
	"exit":      LogExit,
	"req-body":  LogReqBody,
	"req-input": LogReqInput,
	"resp-body": LogRespBody,
	"resp-out":  LogRespOutput,
}

const (
	LogUnknown      LogOption = 0
	LogFull                   = LogEnter | LogExit | LogReqBody | LogReqInput | LogRespBody
	LogFullOnExit             = LogExit | LogReqBody | LogReqInput | LogRespBody
	LogConfidential           = LogExit
)

// Endpoint describes a single REST or WebSocket endpoint and its configuration.
type Endpoint struct {
	// EndpointMeta is the static definition of the endpoint — method, path,
	// required permissions, and log verbosity. It is passed as-is to every
	// middleware so middleware can make decisions based on endpoint configuration
	// without accessing internal state.
	EndpointMeta

	// Compress enables gzip compression of the response. See WithCompress.
	Compress bool

	// ManualStatusCode leaves the success status to the handler
	// (ctx.SetStatusCode). See WithManualStatusCode.
	ManualStatusCode bool

	// SuccessStatusCode is the status of a successful response; zero keeps
	// 200. See WithSuccessStatusCode.
	SuccessStatusCode int

	// ResponseContentType is the Content-Type of a successful response; empty
	// means "application/json; charset=utf-8". See WithContentType.
	ResponseContentType string

	// OptionalBody allows an empty request body on POST/PUT/PATCH.
	// See WithOptionalBody.
	OptionalBody bool

	// Timeout of the request context; zero takes the Vatel default
	// (WithDefaultTimeout), if any. See WithTimeout.
	Timeout time.Duration

	// Controller factories — exactly one must be set.
	Controller          func() Handler
	WebsocketController func() WebsocketHandler

	// Internal — populated by compile().
	v                     *Vatel // shared config and base context
	responseContentType   []byte
	isParamer             bool            // controller implements Paramer
	pathParamKeys         []string        // {name} keys of the path template; logged by decodeParams only
	isQuerier             bool            // controller implements Querier
	isInputFromQuery      bool            // Inputer input is decoded from the query string (GET/DELETE/HEAD/OPTIONS)
	isRequestBodyExpected bool            // Inputer input is decoded from the body (POST/PUT/PATCH)
	hasRespBody           bool            // controller implements Resulter
	middlewares           middlewareSet   // Vatel middleware, copied by BuildHandlers
	localMdw              middlewareSet   // per-endpoint middleware, set via AddMiddleware
	jm                    JsonMasker      // WithJsonMasker, or defaultMasker
	inputFields           jsonmask.Fields // masking rules of Inputer input
	querierMaskFields     jsonmask.Fields // masking rules of Querier input
	resultFieldsByType    *sync.Map       // masking rules of Resulter results: reflect.Type → jsonmask.Fields
	paramFields           []fieldInfo     // precomputed path param layout
	inputQueryFields      []fieldInfo     // precomputed query layout of Inputer input (GET/DELETE/HEAD/OPTIONS)
	querierFields         []fieldInfo     // precomputed query layout of Querier input
	wsLogger              *zerolog.Logger
}

// NewEndpoint builds an Endpoint. Prefer the GET/POST/PUT/PATCH/DELETE builder
// functions from options.go for a more readable call site.
func NewEndpoint(method, path string, perms []string, c func() Handler) *Endpoint {
	return &Endpoint{
		EndpointMeta: EndpointMeta{
			Method: method,
			Path:   path,
			Perms:  append([]string{}, perms...),
		},
		Controller: c,
	}
}

// Endpointer is implemented by any type that exposes a set of endpoints.
type Endpointer interface {
	Endpoints() []Endpoint
}

// Handler is the core interface every controller must implement.
type Handler interface {
	Handle(Context) error
}

// WebsocketHandler is the core interface for WebSocket message controllers.
type WebsocketHandler interface {
	Handle(WebsocketContext) error
}

// Inputer is implemented by controllers that expect decoded query or body input.
// For GET/DELETE/HEAD/OPTIONS the framework decodes URL query parameters (fields tagged
// `param:"name"`); for POST/PUT/PATCH it decodes the JSON body. To read the
// query string on POST/PUT/PATCH as well, implement Querier (embed QueryParams).
// The returned pointer must be stable across the request lifetime (i.e. a field
// of the controller struct, not a local variable).
type Inputer interface {
	Input() any
}

// Querier is implemented by controllers that bind URL query string variables
// (fields tagged `param:"name"`) independently of the request body. The query
// string is decoded for every HTTP method, so a POST controller can implement
// both Querier and Inputer to receive query variables and a JSON body. Embed
// QueryParams to implement it. The returned pointer must be stable across the
// request lifetime.
type Querier interface {
	QueryInput() any
}

// Resulter is implemented by controllers that produce a JSON response body.
// Returning nil from Result() suppresses the response body (useful for 304 / 204).
type Resulter interface {
	Result() any
}

// Paramer is implemented by controllers that bind URL path parameters.
// The returned pointer must be stable across the request lifetime.
type Paramer interface {
	Param() any
}

// Sentinel errors.
var (
	// ErrAuthorizationHeaderMissed was returned for a request without the
	// Authorization header.
	//
	// Deprecated: neither vatel nor vatel/auth returns it; a request without
	// a token gets ErrUnauthorized (VTL-0004). Kept for compatibility.
	ErrAuthorizationHeaderMissed = errors.Template("header Authorization missed").Code("VTL-0001").StatusCode(401).Severity(errors.Medium)
	ErrAccessTokenRevoked        = errors.Template("access token revoked").Code("VTL-0002").StatusCode(401).Severity(errors.Medium)
	ErrForbidden                 = errors.Template("forbidden").Code("VTL-0003").StatusCode(403).Severity(errors.Medium)
	ErrUnauthorized              = errors.Template("unauthorized").Code("VTL-0004").StatusCode(401).Severity(errors.Medium)
	ErrInternalError             = errors.Template("internal error").Code("VTL-0005").StatusCode(500).Severity(errors.Critical)
	ErrValidationFailed          = errors.Template("validation failed").Code("VTL-0006").StatusCode(400).Severity(errors.Tiny)
	ErrInvalidRequestBody        = errors.Template("invalid request body").Code("VTL-0007").StatusCode(400).Severity(errors.Tiny)
	// ErrUnknownPath is sent to a WebSocket client for a message whose path
	// names no WS endpoint.
	ErrUnknownPath = errors.Template("unknown path").Code("VTL-0008").StatusCode(404).Severity(errors.Tiny)
	// ErrWsContextChanged is sent to a WebSocket client for a message whose
	// WsCloseError with WsStatusContextChanged has no error of its own.
	ErrWsContextChanged = errors.Template("security context changed").Code("VTL-0009").StatusCode(409).Severity(errors.Medium)
	// ErrWsTokenConflict is returned by WebsocketVatel.UpdateAccessToken when
	// another payload was stored for the connection, or it closed, while the
	// OnTokenUpdate callback ran.
	ErrWsTokenConflict = errors.Template("concurrent token update").Code("VTL-0010").StatusCode(409).Severity(errors.Medium)
)

// parseLogLevelHeader parses a comma-separated X-Set-Log-Level header value
// into a LogOption bitmask without mutating any shared endpoint state.
func parseLogLevelHeader(raw []byte) LogOption {
	var lo LogOption
	for part := range strings.SplitSeq(string(raw), ",") {
		if v, ok := logOptionString[strings.TrimSpace(part)]; ok {
			lo |= v
		}
	}
	return lo
}

// handler returns the fasthttp handler closure for this endpoint.
func (e *Endpoint) handler(l *zerolog.Logger) func(*fasthttp.RequestCtx) {
	return func(fctx *fasthttp.RequestCtx) {
		verbose := e.v.cfg.verboseError
		lo := e.resolveLogLevel(fctx)

		reqCtx, cancel := e.requestContext()
		defer cancel()
		ctx := newContext(fctx, reqCtx)
		// Runs after the panic recovery below: ctx.Context().Value stops
		// reading fctx before fasthttp reuses it.
		defer ctx.end()
		zco := l.With().Str("client", realip.FromRequest(fctx))
		zc := zco
		// A panic of middleware, decoding, the handler, response writing or a
		// hook is answered with a 500 error; the server keeps running.
		defer func() {
			if r := recover(); r != nil {
				e.recoverPanic(ctx, lo, verbose, zco, r)
			}
		}()

		// reqId is a user value, so every log line of the request gets it
		// once, from the user value visitor.
		if e.v.cfg.logRequestID {
			ctx.Set("reqId", fctx.ID())
		}

		if err := e.runMiddleware(ctx, OnRequest, lo, verbose, zc); err != nil {
			return
		}

		if err := e.runMiddleware(ctx, OnAuthentication, lo, verbose, zc); err != nil {
			return
		}
		// Auth middleware may enable verbose errors for this token/user.
		verbose = verbose || ctx.VerboseError()

		if err := e.runMiddleware(ctx, OnAuthorization, lo, verbose, zc); err != nil {
			return
		}

		zc, h, err := e.buildHandler(fctx, lo, zc)
		if err != nil {
			e.writeErrorResponse(ctx, lo, verbose, zc, err)
			return
		}

		if lo&LogEnter == LogEnter {
			// The decoded params and input are logged once, on the enter line.
			e.logEnter(ctx, zc)
			zc = zco
		}

		if err := e.runMiddleware(ctx, OnHandle, lo, verbose, zc); err != nil {
			return
		}

		if err = h.Handle(ctx); err != nil {
			e.writeErrorResponse(ctx, lo, verbose, zc, err)
			return
		}

		if e.SuccessStatusCode != 0 && !e.ManualStatusCode {
			ctx.SetStatusCode(e.SuccessStatusCode)
		}

		if e.hasRespBody {
			res := h.(Resulter).Result()
			if !isNilResult(res) {
				if zc, err = e.writeResponse(ctx, lo, res, zc); err != nil {
					e.writeErrorResponse(ctx, lo, verbose, zc, err)
					return
				}
			}
		}

		dur := time.Since(fctx.Time())
		if lo&LogExit == LogExit {
			logExit(zc, lo, ctx.VisitUserValues, e.isPathParamKey, dur)
		}
		e.reportMetric(fctx.Response.StatusCode(), dur, len(fctx.Response.Body()))
		ctx.reported = true

		// The response is written: errors of OnSuccessResponse middleware
		// are ignored.
		e.runHooks(ctx, OnSuccessResponse)
	}
}

// requestContext returns the Go context of a request: the Vatel base context,
// with the endpoint timeout if one is set. The returned cancel must be called
// when the request ends.
func (e *Endpoint) requestContext() (context.Context, context.CancelFunc) {
	if e.Timeout > 0 {
		return context.WithTimeout(e.v.baseCtx, e.Timeout)
	}
	return e.v.baseCtx, func() {}
}

// reportMetric reports a finished request to the MetricReporter, if any. The
// size of a HEAD response is 0: the server drops its body.
func (e *Endpoint) reportMetric(statusCode int, dur time.Duration, size int) {
	if e.v.cfg.mr == nil {
		return
	}
	if e.Method == fasthttp.MethodHead {
		size = 0
	}
	e.v.cfg.mr.ReportMetric(e.Method, e.Path, statusCode, dur.Seconds(), size)
}

// alarm passes a 5xx error to the Alarmer, if any.
func (e *Endpoint) alarm(statusCode int, err error) {
	if e.v.cfg.ala != nil && statusCode >= 500 {
		e.v.cfg.ala.Alarm(err)
	}
}

// resolveLogLevel returns the effective LogOption for this request without
// mutating shared endpoint state. The X-Set-Log-Level header replaces the
// endpoint level, unless WithStaticLoggingLevel is set or the endpoint is
// LogSilent: the header cannot turn LogSilent off.
func (e *Endpoint) resolveLogLevel(fctx *fasthttp.RequestCtx) LogOption {
	lo := e.LogOptions
	if !e.v.cfg.staticLoggingLevel {
		if raw := fctx.Request.Header.Peek("X-Set-Log-Level"); len(raw) > 0 {
			lo = parseLogLevelHeader(raw) | e.LogOptions&LogSilent
		}
	}
	return effectiveLogLevel(lo)
}

// effectiveLogLevel applies the precedence of LogSilent: when it is set, all
// other bits are dropped.
func effectiveLogLevel(lo LogOption) LogOption {
	if lo&LogSilent != 0 {
		return LogSilent
	}
	return lo
}

// isErrorLogged reports whether an error response with statusCode is logged
// at level lo: LogSilent logs 5xx errors only.
func isErrorLogged(lo LogOption, statusCode int) bool {
	return lo&LogSilent == 0 || statusCode >= 500
}

// AddMiddleware adds per-endpoint middleware at the given pipeline position.
// Called from EndpointOption functions (e.g. auth.RequirePerms).
func (e *Endpoint) AddMiddleware(pos MiddlewarePos, f ...func(Context, EndpointMeta) error) {
	e.localMdw[pos] = append(e.localMdw[pos], f...)
}

// middlewareAt yields the global, then the per-endpoint middleware at pos.
func (e *Endpoint) middlewareAt(pos MiddlewarePos) iter.Seq[func(Context, EndpointMeta) error] {
	return func(yield func(func(Context, EndpointMeta) error) bool) {
		for _, mws := range [2][]func(Context, EndpointMeta) error{e.middlewares[pos], e.localMdw[pos]} {
			for _, mw := range mws {
				if !yield(mw) {
					return
				}
			}
		}
	}
}

// runMiddleware executes global then per-endpoint middleware at pos in order.
// On the first error it writes the error response and returns it.
func (e *Endpoint) runMiddleware(ctx *VatelContext, pos MiddlewarePos, lo LogOption, verbose bool, zc zerolog.Context) error {
	for mw := range e.middlewareAt(pos) {
		if err := mw(ctx, e.EndpointMeta); err != nil {
			e.writeErrorResponse(ctx, lo, verbose, zc, err)
			return err
		}
	}
	return nil
}

// runHooks executes global then per-endpoint middleware at pos, ignoring their
// errors: it is used after the response is written (OnSuccessResponse,
// OnErrorResponse).
func (e *Endpoint) runHooks(ctx Context, pos MiddlewarePos) {
	for mw := range e.middlewareAt(pos) {
		mw(ctx, e.EndpointMeta) //nolint:errcheck
	}
}

// isPathParamKey reports whether the user value key is a path param of the
// endpoint. Path params are logged (masked) by decodeParams, so user value
// visitors skip them.
func (e *Endpoint) isPathParamKey(key []byte) bool {
	for _, k := range e.pathParamKeys {
		if string(key) == k {
			return true
		}
	}
	return false
}

// withUserValues adds the values visit yields to zc, except keys for which
// skip (if not nil) returns true.
func withUserValues(zc zerolog.Context, visit func(func([]byte, any)), skip func([]byte) bool) zerolog.Context {
	visit(func(key []byte, v any) {
		if skip == nil || !skip(key) {
			zc = zc.Interface(string(key), v)
		}
	})
	return zc
}

// logEnter logs the "new request" line with zc and the user values of the
// request.
func (e *Endpoint) logEnter(ctx Context, zc zerolog.Context) {
	zl := withUserValues(zc, ctx.VisitUserValues, e.isPathParamKey).Logger()
	zl.Debug().Msg("new request")
}

// logExit logs the exit line ("processed", or "completed" with LogEnter) of an
// HTTP request or a WS message with its duration and the values visit yields,
// except keys for which skip returns true. A "message" value replaces the
// message unless skip returns true for it (a path param named "message" is
// logged masked by decodeParams, never as the raw message).
func logExit(zc zerolog.Context, lo LogOption, visit func(func([]byte, any)), skip func([]byte) bool, dur time.Duration) {
	msg := "completed"
	if lo&LogEnter != LogEnter {
		msg = "processed"
	}
	withoutMessage := func(f func([]byte, any)) {
		visit(func(key []byte, v any) {
			if skip != nil && skip(key) {
				return
			}
			if string(key) == "message" {
				msg = fmt.Sprint(v)
				return
			}
			f(key, v)
		})
	}
	zl := withUserValues(zc, withoutMessage, nil).Logger()
	zl.Debug().Str("dur", dur.String()).Msg(msg)
}

// buildHandler instantiates the controller, decodes path params, Querier query
// input and Inputer query/body input using precomputed field maps, then runs
// Validator on the Querier input and the Inputer input, in that order.
func (e *Endpoint) buildHandler(fctx *fasthttp.RequestCtx, lo LogOption, zc zerolog.Context) (zerolog.Context, Handler, error) {
	h := e.Controller()
	var err error

	if e.isParamer {
		p := h.(Paramer).Param()
		if zc, err = decodeParams(fctx, p, e.paramFields, zc); err != nil {
			return zc, nil, err
		}
	}

	var query, in any

	if e.isQuerier {
		query = h.(Querier).QueryInput()
		if zc, err = decodeURLQuery(fctx, query, e.querierFields, zc); err != nil {
			return zc, nil, err
		}
		if lo&LogReqInput == LogReqInput {
			zc = e.logValue(zc, "reqQuery", query, e.querierMaskFields)
		}
	}

	if e.isInputFromQuery {
		in = h.(Inputer).Input()
		if zc, err = decodeURLQuery(fctx, in, e.inputQueryFields, zc); err != nil {
			return zc, nil, err
		}
		if lo&LogReqInput == LogReqInput {
			zc = e.logValue(zc, "reqInput", in, e.inputFields)
		}
	}

	if e.isRequestBodyExpected {
		body := fctx.Request.Body()
		bodyIn := h.(Inputer).Input()

		decoded, err := decodeBody(body, bodyIn, e.OptionalBody, e.inputFields.HasRules())
		// A skipped optional body has nothing to log.
		if lo&LogReqBody == LogReqBody && (decoded || err != nil) {
			zc = e.logRequestBody(zc, body, bodyIn, decoded)
		}
		if err != nil {
			return zc, nil, err
		}
		if decoded {
			in = bodyIn
			if lo&LogReqInput == LogReqInput {
				zc = e.logValue(zc, "reqInput", in, e.inputFields)
			}
		}
	}

	// Validation runs after all input is decoded: Querier input first, then
	// Inputer input. A skipped optional body leaves in nil, so it is not
	// validated.
	if query != nil {
		if err := validate(query); err != nil {
			return zc, nil, err
		}
	}
	if in != nil {
		if err := validate(in); err != nil {
			return zc, nil, err
		}
	}

	return zc, h, nil
}

// logRequestBody adds the request body to zc under "requestBody". A body of an
// input type without `mask` tags is logged compacted. A body of an input type
// with `mask` tags is logged as the decoded input, marshalled and masked: the
// raw body may carry a secret under a key the masker does not match but
// encoding/json decodes (another letter case, a duplicate key), so it is
// never logged; when it cannot be decoded only an error marker is logged.
func (e *Endpoint) logRequestBody(zc zerolog.Context, body []byte, in any, decoded bool) zerolog.Context {
	if e.inputFields.HasRules() {
		if !decoded {
			return zc.Str("requestBodyLoggingFailed", "not logged: the body cannot be decoded and its type has masked fields")
		}
		return e.logValue(zc, "requestBody", in, e.inputFields)
	}
	compact := bytes.NewBuffer(nil)
	if jerr := json.Compact(compact, body); jerr != nil {
		return zc.Str("requestBodyLoggingFailed", jerr.Error())
	}
	return zc.RawJSON("requestBody", compact.Bytes())
}

// logValue adds v to zc under key. A type without `mask` tags is logged as
// is; otherwise v is marshalled and masked.
func (e *Endpoint) logValue(zc zerolog.Context, key string, v any, fields jsonmask.Fields) zerolog.Context {
	if !fields.HasRules() {
		return zc.Interface(key, v)
	}
	buf, err := json.Marshal(v)
	if err != nil {
		return zc.Str(key+"LoggingFailed", err.Error())
	}
	return e.logJSON(zc, key, buf, fields)
}

// logJSON adds the JSON document buf, masked with fields, to zc under key.
// When masking fails only the error is logged, never the unmasked document.
func (e *Endpoint) logJSON(zc zerolog.Context, key string, buf []byte, fields jsonmask.Fields) zerolog.Context {
	if fields.HasRules() {
		masked, err := e.jm.Mask(buf, fields)
		if err != nil {
			return zc.Str(key+"MaskingFailed", err.Error())
		}
		buf = masked
	}
	return zc.RawJSON(key, buf)
}

// writeResponse marshals res and writes it to the response body,
// optionally logging and masking sensitive fields.
func (e *Endpoint) writeResponse(ctx Context, lo LogOption, res any, zc zerolog.Context) (zerolog.Context, error) {
	ctx.SetContentType(e.responseContentType)
	zc, _, err := e.writeJSON(ctx.BodyWriter(), lo, res, zc)
	return zc, err
}

// isNilResult reports whether a value returned from Resulter.Result() is nil,
// including typed nils (e.g. (*T)(nil) wrapped in an interface).
func isNilResult(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// writeJSON is the shared marshal-mask-log-write implementation used by both
// HTTP and WebSocket response paths. The marshalled response is masked and
// logged under "respBody" (LogRespBody) or "result" (LogRespOutput only); the
// client receives it unmasked. It returns zc with the logged response and
// the size of the response. A marshalling error is returned without logging
// the response.
func (e *Endpoint) writeJSON(w io.Writer, lo LogOption, res any, zc zerolog.Context) (zerolog.Context, int, error) {
	buf, err := json.Marshal(res)
	if err != nil {
		return zc, 0, err
	}

	if lo&(LogRespOutput|LogRespBody) != 0 {
		key := "respBody"
		if lo&LogRespBody == 0 {
			key = "result"
		}
		zc = e.logJSON(zc, key, buf, e.resultRules(res))
	}

	_, err = w.Write(buf)
	return zc, len(buf), err
}

// resultRules returns the masking rules of the result res. They follow the
// type of res: Result() may return a result of another type than at compile
// time (an untyped nil, as OptionalOutput does before the handler sets Out, or
// any value of an interface-typed field). Rules of a type are built on first
// use and cached.
func (e *Endpoint) resultRules(res any) jsonmask.Fields {
	t := reflect.TypeOf(res)
	if f, ok := e.resultFieldsByType.Load(t); ok {
		return f.(jsonmask.Fields)
	}
	f := e.jm.Fields(res, maskTag)
	e.resultFieldsByType.Store(t, f)
	return f
}

// writeErrorResponse logs the error (at level lo), writes an error JSON body,
// reports metrics, triggers the alarm if critical, and runs OnErrorResponse
// middleware with the endpoint metadata.
func (e *Endpoint) writeErrorResponse(ctx *VatelContext, lo LogOption, verbose bool, zc zerolog.Context, err error) {
	if err == nil {
		return
	}

	statusCode, size := e.writeError(ctx, lo, verbose, zc, err)
	e.reportMetric(statusCode, time.Since(ctx.RequestTime()), size)
	ctx.reported = true
	e.alarm(statusCode, err)
	e.runHooks(ctx, OnErrorResponse)
}

// recoverPanic answers a request whose pipeline panicked with r. The response
// written so far is dropped and replaced with ErrInternalError (500, VTL-0005;
// the panic value reaches the client only when verbose is set). The panic
// value and the stack are logged, the Alarmer is called, and, unless the
// request was reported already (a panic of a hook that runs after the
// response is written), the metric is reported and OnErrorResponse middleware
// runs. A panic during the recovery itself is logged and dropped.
func (e *Endpoint) recoverPanic(ctx *VatelContext, lo LogOption, verbose bool, zc zerolog.Context, r any) {
	defer func() {
		if r := recover(); r != nil {
			zl := withPanic(zc, r).Logger()
			zl.Error().Msg("panic while recovering from a panic")
		}
	}()

	err := panicError(r)
	reported := ctx.reported
	ctx.fh.Response.Reset()
	statusCode, size := e.writeError(ctx, lo, verbose, withPanic(zc, r), err)
	if !reported {
		e.reportMetric(statusCode, time.Since(ctx.RequestTime()), size)
		ctx.reported = true
	}
	e.alarm(statusCode, err)
	if !reported {
		e.runHooks(ctx, OnErrorResponse)
	}
}

// panicError returns the error of a recovered panic value r: ErrInternalError
// wrapping "panic: <r>".
func panicError(r any) error {
	if err, ok := r.(error); ok {
		return ErrInternalError.Wrap(fmt.Errorf("panic: %w", err))
	}
	return ErrInternalError.Wrap(fmt.Errorf("panic: %v", r))
}

// withPanic adds the panic value r and the stack of the panicking goroutine
// to zc. It is called from a deferred function, so the stack includes the
// frames that panicked.
func withPanic(zc zerolog.Context, r any) zerolog.Context {
	return zc.Str("panic", fmt.Sprint(r)).Str("stack", string(debug.Stack()))
}

// writeError logs the error (at level lo) and writes the error JSON body with
// its status. It returns the status and the size of the body.
func (e *Endpoint) writeError(ctx *VatelContext, lo LogOption, verbose bool, zc zerolog.Context, err error) (int, int) {
	statusCode, body, xe := e.errorResponse(err, verbose)
	if statusCode == fasthttp.StatusTooManyRequests && xe != nil {
		if ra, ok := errors.Serialize(xe, errors.WithAttributes(errors.AddFields)).Fields["Retry-After"]; ok {
			var hv []byte
			switch v := ra.(type) {
			case string:
				hv = []byte(v)
			case []byte:
				hv = v
			default:
				hv = []byte(fmt.Sprint(ra))
			}
			ctx.SetHeader([]byte("Retry-After"), hv)
		}
	}

	if isErrorLogged(lo, statusCode) {
		zl := withUserValues(zc, ctx.VisitUserValues, e.isPathParamKey).
			RawJSON("err", errors.ToJSON(err, errors.WithAttributes(errors.ServerOutputFormat))).Logger()
		zl.Error().Msg("request failed")
	}

	ctx.SetContentType([]byte(jsonContentType))
	ctx.SetStatusCode(statusCode)
	ctx.BodyWriter().Write(body) //nolint:errcheck
	return statusCode, len(body)
}

// errorResponse returns the status code of err and the error JSON sent to the
// client (see Vatel.errorResponse).
func (e *Endpoint) errorResponse(err error, verbose bool) (statusCode int, body []byte, xe *errors.Error) {
	return e.v.errorResponse(err, verbose)
}

// errorResponse returns the status code of err and the error JSON sent to the
// client.
//
// The status and the client error come from the first *errors.Error (or
// *errors.ErrorTemplate) in the chain of err, so a fmt.Errorf("...: %w")
// wrapper keeps the status of the error it wraps and its text is not sent.
// Without one, or when it has no status, the status is defaultErrorStatus.
//
// An error chain without *errors.Error carries internal text (driver errors,
// file paths). Unless verbose is set, the client gets a generic error of the
// status instead ("internal error", VTL-0005, for 500); err itself is logged.
func (v *Vatel) errorResponse(err error, verbose bool) (statusCode int, body []byte, xe *errors.Error) {
	xe = vatelError(err)
	var client error = err
	if xe != nil {
		statusCode = errors.Serialize(xe).StatusCode
		client = xe
	}
	if statusCode == 0 {
		statusCode = defaultErrorStatus(err)
	}
	if xe == nil && !verbose {
		client = genericError(statusCode)
	}

	var ff errors.ErrorSerializationRule
	if verbose {
		ff = errors.AddStack | errors.AddFields | errors.AddWrappedErrors
	}
	return statusCode, errors.ToJSON(client, errors.WithAttributes(ff), errors.WithRootLevelFields(v.cfg.clientErrorFields)), xe
}

// vatelError returns the first *errors.Error in the chain of err, or a new
// error of the first *errors.ErrorTemplate, or nil.
func vatelError(err error) *errors.Error {
	var xe *errors.Error
	if stderrors.As(err, &xe) {
		return xe
	}
	var xt *errors.ErrorTemplate
	if stderrors.As(err, &xt) {
		return xt.New()
	}
	return nil
}

// genericError returns the error sent to the client instead of an error
// without *errors.Error in its chain.
func genericError(statusCode int) *errors.Error {
	if statusCode == fasthttp.StatusInternalServerError {
		return ErrInternalError.New()
	}
	return errors.Template(strings.ToLower(fasthttp.StatusMessage(statusCode))).StatusCode(statusCode).New()
}

// defaultErrorStatus returns the HTTP status of an error without its own
// status code: 504 for a context deadline, 503 for a cancelled context
// (Vatel.Shutdown), 500 otherwise.
func defaultErrorStatus(err error) int {
	switch {
	case stderrors.Is(err, context.DeadlineExceeded):
		return fasthttp.StatusGatewayTimeout
	case stderrors.Is(err, context.Canceled):
		return fasthttp.StatusServiceUnavailable
	}
	return fasthttp.StatusInternalServerError
}

// compile validates the endpoint configuration and precomputes all reflection
// data needed at request time. Called once per endpoint during BuildHandlers.
func (e *Endpoint) compile(v *Vatel) error {
	origPath := e.Path
	e.Path = path.Join(v.cfg.urlPrefix, e.Path)

	if !strings.HasPrefix(e.Path, "/") {
		return fmt.Errorf("endpoint %s %s: path must begin with '/'", e.Method, origPath)
	}

	compileCommon(v, e)

	if e.ResponseContentType != "" {
		e.responseContentType = []byte(e.ResponseContentType)
	} else {
		e.responseContentType = []byte(jsonContentType)
	}

	if e.Controller == nil {
		return fmt.Errorf("endpoint %s %s: missing Controller", e.Method, origPath)
	}
	c := e.Controller()
	if isNilResult(c) {
		return fmt.Errorf("endpoint %s %s: Controller returned nil", e.Method, origPath)
	}
	if fh, ok := c.(*funcHandler); ok && fh.f == nil {
		return fmt.Errorf("endpoint %s %s: Func(nil)", e.Method, origPath)
	}

	// Validate path param / Paramer consistency.
	e.pathParamKeys = parsePathParamKeys(e.Path)
	pathHasParam := len(e.pathParamKeys) > 0
	_, isParamer := c.(Paramer)
	// A Func handler reads path params with Context.PathValue.
	_, isFunc := c.(*funcHandler)

	if !isParamer && !isFunc && pathHasParam {
		return fmt.Errorf("endpoint %s %s: path has parameters but controller does not implement Paramer", e.Method, origPath)
	}
	if isParamer && !pathHasParam {
		return fmt.Errorf("endpoint %s %s: controller implements Paramer but path has no parameters", e.Method, origPath)
	}

	e.isParamer = isParamer
	if isParamer {
		fields, err := buildParamFields(c.(Paramer).Param())
		if err != nil {
			return fmt.Errorf("endpoint %s %s: %w", e.Method, origPath, err)
		}
		e.paramFields = fields
	}

	_, hasRespBody := c.(Resulter)
	e.hasRespBody = hasRespBody
	e.compileMask(c)

	ii, isInputer := c.(Inputer)

	// Querier input is decoded from the URL query string for every method.
	qi, isQuerier := c.(Querier)
	e.isQuerier = isQuerier
	if isQuerier {
		fields, err := buildQueryFields(qi.QueryInput(), "QueryInput()")
		if err != nil {
			return fmt.Errorf("endpoint %s %s: %w", e.Method, origPath, err)
		}
		e.querierFields = fields
		e.querierMaskFields = e.jm.Fields(qi.QueryInput(), maskTag)
	}

	// Route input decoding based on HTTP method.
	switch e.Method {
	case fasthttp.MethodGet, fasthttp.MethodDelete, fasthttp.MethodHead, fasthttp.MethodOptions:
		if e.OptionalBody {
			return fmt.Errorf("endpoint %s %s: WithOptionalBody is not allowed for a method without a request body", e.Method, origPath)
		}
		e.isInputFromQuery = isInputer
		if isInputer {
			fields, err := buildQueryFields(ii.Input(), "Input()")
			if err != nil {
				return fmt.Errorf("endpoint %s %s: %w", e.Method, origPath, err)
			}
			e.inputQueryFields = fields
		}
	case fasthttp.MethodPost, fasthttp.MethodPut, fasthttp.MethodPatch:
		e.isRequestBodyExpected = isInputer
	default:
		return fmt.Errorf("endpoint %s %s: unknown HTTP method", e.Method, origPath)
	}

	return nil
}

// parsePathParamKeys returns the param names of a fasthttp/router path
// template: "id" for {id}, {id?}, {id:regex} and {id:*}. Braces inside a regex
// are balanced.
func parsePathParamKeys(p string) []string {
	var keys []string
	for i := 0; i < len(p); i++ {
		if p[i] != '{' {
			continue
		}
		start := i + 1
		end := start
		for end < len(p) && p[end] != '}' && p[end] != ':' && p[end] != '?' {
			end++
		}
		if end > start {
			keys = append(keys, p[start:end])
		}
		// Skip to the matching closing brace.
		depth := 1
		for i = start; i < len(p) && depth > 0; i++ {
			switch p[i] {
			case '{':
				depth++
			case '}':
				depth--
			}
		}
		i--
	}
	return keys
}

// compileMask precomputes masking rules of the Inputer input and the Resulter
// result of controller c. Rules of a result type unknown at compile time
// (Result() returns an untyped nil or a value of another type) are built per
// type on first use.
func (e *Endpoint) compileMask(c any) {
	if ri, ok := c.(Resulter); ok {
		e.resultFieldsByType = &sync.Map{}
		if res := ri.Result(); res != nil {
			e.resultFieldsByType.Store(reflect.TypeOf(res), e.jm.Fields(res, maskTag))
		}
	}
	if ii, ok := c.(Inputer); ok {
		e.inputFields = e.jm.Fields(ii.Input(), maskTag)
	}
}

// maskTag is the struct tag of masking rules.
const maskTag = "mask"

// jsonContentType is the Content-Type of JSON responses and error responses.
const jsonContentType = "application/json; charset=utf-8"

// defaultMasker masks log output of endpoints when WithJsonMasker is not set.
var defaultMasker = jsonmask.New()

// compileCommon links the endpoint to the shared Vatel config and resolves
// the per-endpoint defaults. The Vatel middleware set is copied: middleware
// added after BuildHandlers has no effect.
func compileCommon(v *Vatel, e *Endpoint) {
	e.v = v
	e.middlewares = v.mdw
	if e.Timeout == 0 {
		e.Timeout = v.cfg.defaultTimeout
	}
	e.jm = v.cfg.jm
	if e.jm == nil {
		e.jm = defaultMasker
	}
	if e.LogOptions == LogUnknown {
		e.LogOptions = v.cfg.defaultLogOption
	}
}
