package vatel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/axkit/errors"
	realip "github.com/axkit/fasthttp-realip"
	"github.com/axkit/vatel/jsonmask"
	"github.com/regorov/websocket"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

// LogOption is a bitmask controlling per-endpoint log verbosity.
type LogOption uint32

const (
	LogSilent     LogOption = 1 << iota // suppress all logging for this endpoint
	LogEnter                            // log when a request arrives
	LogExit                             // log when a request completes
	LogReqBody                          // log the raw request body (masked by `mask` tags)
	LogReqInput                         // log the decoded input struct (masked by `mask` tags)
	LogRespBody                         // log the raw response body (masked by `mask` tags)
	LogRespOutput                       // log the response struct (masked by `mask` tags)
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

	// Additional public configuration fields.
	Compress            bool
	NoInputLog          bool
	NoResultLog         bool
	ManualStatusCode    bool
	SuccessStatusCode   int
	ResponseContentType string
	LanguageLabel       string

	// OptionalBody allows an empty request body on POST/PUT/PATCH.
	// See WithOptionalBody.
	OptionalBody bool

	// Controller factories — exactly one must be set.
	Controller          func() Handler
	WebsocketController func() WebsocketHandler

	// Internal — populated by compile().
	staticLoggingLevel    bool
	verboseError          bool
	logRequestID          bool
	clientErrorFields     []string
	responseContentType   []byte
	isPathParametrized    bool
	isQuerier             bool // controller implements Querier
	isURLQueryExpected    bool
	isRequestBodyExpected bool
	hasRespBody           bool
	middlewares           middlewareSet
	localMdw              middlewareSet // per-endpoint middleware, set via AddMiddleware
	jm                    JsonMasker
	inputFields           jsonmask.Fields // masking rules of Inputer input
	queryMaskFields       jsonmask.Fields // masking rules of Querier input
	resultFields          jsonmask.Fields // masking rules of Resulter result
	resultFieldsDynamic   bool            // result type unknown at compile: rules are built per response
	paramFields           []fieldInfo     // precomputed path param layout
	queryFields           []fieldInfo     // precomputed query layout of Inputer input (GET/DELETE/HEAD/OPTIONS)
	querierFields         []fieldInfo     // precomputed query layout of Querier input
	ala                   Alarmer
	mr                    MetricReporter
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
	Input() interface{}
}

// Querier is implemented by controllers that bind URL query string variables
// (fields tagged `param:"name"`) independently of the request body. The query
// string is decoded for every HTTP method, so a POST controller can implement
// both Querier and Inputer to receive query variables and a JSON body. Embed
// QueryParams to implement it. The returned pointer must be stable across the
// request lifetime.
type Querier interface {
	QueryInput() interface{}
}

// Resulter is implemented by controllers that produce a JSON response body.
// Returning nil from Result() suppresses the response body (useful for 304 / 204).
type Resulter interface {
	Result() interface{}
}

// Paramer is implemented by controllers that bind URL path parameters.
// The returned pointer must be stable across the request lifetime.
type Paramer interface {
	Param() interface{}
}

// Sentinel errors.
var (
	ErrAuthorizationHeaderMissed = errors.Template("header Authorization missed").Code("VTL-0001").StatusCode(401).Severity(errors.Medium)
	ErrAccessTokenRevoked        = errors.Template("access token revoked").Code("VTL-0002").StatusCode(401).Severity(errors.Medium)
	ErrForbidden                 = errors.Template("forbidden").Code("VTL-0003").StatusCode(403).Severity(errors.Medium)
	ErrUnauthorized              = errors.Template("unauthorized").Code("VTL-0004").StatusCode(401).Severity(errors.Medium)
	ErrInternalError             = errors.Template("internal error").Code("VTL-0005").StatusCode(500).Severity(errors.Critical)
	ErrValidationFailed          = errors.Template("validation failed").Code("VTL-0006").StatusCode(400).Severity(errors.Tiny)
	ErrInvalidRequestBody        = errors.Template("invalid request body").Code("VTL-0007").StatusCode(400).Severity(errors.Tiny)
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
		verbose := e.verboseError
		lo := e.resolveLogLevel(fctx)

		ctx := NewContext(fctx)
		zco := l.With().Str("client", realip.FromRequest(fctx))
		if e.logRequestID {
			zco = zco.Uint64("reqId", fctx.ID())
			ctx.Set("reqId", fctx.ID())
		}
		zc := zco

		if err := e.runMiddleware(ctx, OnRequest, verbose, &zc, e.EndpointMeta); err != nil {
			return
		}

		if err := e.runMiddleware(ctx, OnAuthentication, verbose, &zc, e.EndpointMeta); err != nil {
			return
		}
		// Auth middleware may enable verbose errors for this token/user.
		verbose = verbose || ctx.VerboseError()

		if err := e.runMiddleware(ctx, OnAuthorization, verbose, &zc, e.EndpointMeta); err != nil {
			return
		}

		zc, h, err := e.buildHandler(fctx, lo, zc)
		if err != nil {
			e.writeErrorResponse(ctx, verbose, &zc, err, e.EndpointMeta)
			return
		}

		if lo&LogEnter == LogEnter {
			e.logEnter(ctx, &zc, zco)
		}

		if err := e.runMiddleware(ctx, OnHandle, verbose, &zc, e.EndpointMeta); err != nil {
			return
		}

		if err = h.Handle(ctx); err != nil {
			e.writeErrorResponse(ctx, verbose, &zc, err, e.EndpointMeta)
			return
		}

		if e.SuccessStatusCode != 0 && !e.ManualStatusCode {
			ctx.SetStatusCode(e.SuccessStatusCode)
		}

		if e.hasRespBody {
			res := h.(Resulter).Result()
			if !isNilResult(res) {
				if err := e.writeResponse(ctx, lo, res, &zc); err != nil {
					e.writeErrorResponse(ctx, verbose, &zc, err, e.EndpointMeta)
					return
				}
			}
		}

		dur := time.Since(fctx.Time())
		if lo&LogExit == LogExit {
			e.logExit(ctx, lo, &zc, dur)
		}
		if e.mr != nil {
			e.mr.ReportMetric(e.Method, e.Path, fctx.Response.StatusCode(), dur.Seconds(), len(fctx.Response.Body()))
		}

		e.runMiddleware(ctx, OnSuccessResponse, verbose, &zc, e.EndpointMeta) //nolint:errcheck
	}
}

// resolveLogLevel returns the effective LogOption for this request without
// mutating shared endpoint state.
func (e *Endpoint) resolveLogLevel(fctx *fasthttp.RequestCtx) LogOption {
	if e.staticLoggingLevel {
		return e.LogOptions
	}
	if raw := fctx.Request.Header.Peek("X-Set-Log-Level"); len(raw) > 0 {
		return parseLogLevelHeader(raw)
	}
	return LogOption(atomic.LoadUint32((*uint32)(&e.LogOptions)))
}

// AddMiddleware adds per-endpoint middleware at the given pipeline position.
// Called from EndpointOption functions (e.g. auth.RequirePerms).
func (e *Endpoint) AddMiddleware(pos MiddlewarePos, f ...func(Context, EndpointMeta) error) {
	e.localMdw[pos] = append(e.localMdw[pos], f...)
}

// runMiddleware executes global then per-endpoint middleware at pos in order.
// On the first error it writes the error response and returns it.
func (e *Endpoint) runMiddleware(ctx Context, pos MiddlewarePos, verbose bool, zc *zerolog.Context, meta EndpointMeta) error {
	for _, mw := range e.middlewares[pos] {
		if err := mw(ctx, e.EndpointMeta); err != nil {
			e.writeErrorResponse(ctx, verbose, zc, err)
			return err
		}
	}
	for _, mw := range e.localMdw[pos] {
		if err := mw(ctx, e.EndpointMeta); err != nil {
			e.writeErrorResponse(ctx, verbose, zc, err)
			return err
		}
	}
	return nil
}

// logEnter logs an "enter" event enriched with current user values, then resets
// zc back to the base context (zco) so subsequent log entries stay clean.
func (e *Endpoint) logEnter(ctx Context, zc *zerolog.Context, zco zerolog.Context) {
	ctx.VisitUserValues(func(key []byte, v any) {
		*zc = (*zc).Interface(string(key), v)
	})
	zl := zc.Logger()
	zl.Debug().Msg("new request")
	*zc = zco
}

// logExit logs an "exit" event with duration and any user values set during the
// request.
func (e *Endpoint) logExit(ctx Context, lo LogOption, zc *zerolog.Context, dur time.Duration) {
	msg := "completed"
	if lo&LogEnter != LogEnter {
		msg = "processed"
	}
	ctx.VisitUserValues(func(key []byte, v any) {
		if bytes.Equal(key, []byte("message")) {
			msg = v.(string)
			return
		}
		*zc = (*zc).Interface(string(key), v)
	})
	zl := zc.Logger()
	zl.Debug().Str("dur", dur.String()).Msg(msg)
}

// buildHandler instantiates the controller, decodes path params, Querier query
// input and Inputer query/body input using precomputed field maps, then runs
// Validator on the Querier input and the Inputer input, in that order.
func (e *Endpoint) buildHandler(fctx *fasthttp.RequestCtx, lo LogOption, zc zerolog.Context) (zerolog.Context, Handler, error) {
	h := e.Controller()
	var err error

	if e.isPathParametrized {
		p := h.(Paramer).Param()
		if zc, err = decodeParams(fctx, p, e.paramFields, zc); err != nil {
			return zc, nil, err
		}
	}

	var query, in interface{}

	if e.isQuerier {
		query = h.(Querier).QueryInput()
		if zc, err = decodeURLQuery(fctx, query, e.querierFields, zc); err != nil {
			return zc, nil, err
		}
		if lo&LogReqInput == LogReqInput {
			zc = e.logValue(zc, "reqQuery", query, e.queryMaskFields)
		}
	}

	if e.isURLQueryExpected {
		in = h.(Inputer).Input()
		if zc, err = decodeURLQuery(fctx, in, e.queryFields, zc); err != nil {
			return zc, nil, err
		}
		if lo&LogReqInput == LogReqInput {
			zc = e.logValue(zc, "reqInput", in, e.inputFields)
		}
	}

	if e.isRequestBodyExpected {
		body := fctx.Request.Body()
		bodyIn := h.(Inputer).Input()

		decoded, err := decodeBody(body, bodyIn, e.OptionalBody)
		// A skipped optional body has nothing to log.
		if lo&LogReqBody == LogReqBody && (decoded || err != nil) {
			zc = e.logRequestBody(zc, body)
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

// logRequestBody adds the compacted request body, masked by the `mask` tags
// of the input type, to zc.
func (e *Endpoint) logRequestBody(zc zerolog.Context, body []byte) zerolog.Context {
	compact := bytes.NewBuffer(nil)
	if jerr := json.Compact(compact, body); jerr != nil {
		return zc.Str("requestBodyLoggingFailed", jerr.Error())
	}
	return e.logJSON(zc, "requestBody", compact.Bytes(), e.inputFields)
}

// logValue adds v to zc under key. A type without `mask` tags is logged as
// is; otherwise v is marshalled and masked.
func (e *Endpoint) logValue(zc zerolog.Context, key string, v interface{}, fields jsonmask.Fields) zerolog.Context {
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
func (e *Endpoint) writeResponse(ctx Context, lo LogOption, res interface{}, zc *zerolog.Context) error {
	ctx.SetContentType(e.responseContentType)
	return e.writeJSON(ctx.BodyWriter(), lo, res, zc)
}

// wsWriteResponse marshals res and writes it to a WebSocket connection,
// optionally logging and masking sensitive fields.
func (e *Endpoint) wsWriteResponse(c *websocket.Conn, lo LogOption, res interface{}, zc *zerolog.Context) error {
	return e.writeJSON(c, lo, res, zc)
}

// isNilResult reports whether a value returned from Resulter.Result() is nil,
// including typed nils (e.g. (*T)(nil) wrapped in an interface).
func isNilResult(v interface{}) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Ptr && rv.IsNil()
}

// writeJSON is the shared marshal-mask-log-write implementation used by both
// HTTP and WebSocket response paths. The marshalled response is masked once
// and logged under "result" and/or "respBody"; the client receives it
// unmasked. A marshalling error is returned without logging the response.
func (e *Endpoint) writeJSON(w io.Writer, lo LogOption, res interface{}, zc *zerolog.Context) error {
	buf, err := json.Marshal(res)
	if err != nil {
		return err
	}

	if lo&(LogRespOutput|LogRespBody) != 0 {
		fields := e.resultFields
		if e.resultFieldsDynamic && e.jm != nil {
			fields = e.jm.Fields(res, "mask")
		}
		logged := buf
		if fields.HasRules() {
			if logged, err = e.jm.Mask(buf, fields); err != nil {
				*zc = zc.Str("respMaskingFailed", err.Error())
				logged = nil
			}
		}
		if logged != nil {
			if lo&LogRespOutput == LogRespOutput {
				*zc = zc.RawJSON("result", logged)
			}
			if lo&LogRespBody == LogRespBody {
				*zc = zc.RawJSON("respBody", logged)
			}
		}
	}

	_, err = w.Write(buf)
	return err
}

// writeErrorResponse logs the error, writes an error JSON body, reports metrics,
// triggers the alarm if critical, and runs OnErrorResponse middleware.
func (e *Endpoint) writeErrorResponse(ctx Context, verbose bool, zc *zerolog.Context, err error, meta ...EndpointMeta) {
	if err == nil {
		return
	}

	statusCode := 500

	if xe, ok := err.(*errors.Error); ok {
		ce := errors.Serialize(xe, errors.WithAttributes(errors.ServerOutputFormat))
		statusCode = ce.StatusCode
		if statusCode == 429 {
			if ra, ok := ce.Fields["Retry-After"]; ok {
				var hv []byte
				switch v := ra.(type) {
				case string:
					hv = []byte(v)
				case []byte:
					hv = v
				default:
					hv = []byte(fmt.Sprintf("%d", ra))
				}
				ctx.SetHeader([]byte("Retry-After"), hv)
			}
		}
	}

	z := *zc
	ctx.VisitUserValues(func(key []byte, v interface{}) {
		z = z.Interface(string(key), v)
	})
	zl := z.RawJSON("err", errors.ToJSON(err, errors.WithAttributes(errors.ServerOutputFormat))).Logger()
	zl.Error().Msg("request failed")

	ctx.SetContentType([]byte("application/json; charset=utf-8"))
	ctx.SetStatusCode(statusCode)

	var ff errors.ErrorSerializationRule
	if verbose {
		ff = errors.AddStack | errors.AddFields | errors.AddWrappedErrors
	}
	buf := errors.ToJSON(err, errors.WithAttributes(ff), errors.WithRootLevelFields(e.clientErrorFields))
	ctx.BodyWriter().Write(buf) //nolint:errcheck

	if e.mr != nil {
		e.mr.ReportMetric(e.Method, e.Path, statusCode, time.Since(ctx.RequestTime()).Seconds(), len(buf))
	}

	if e.ala != nil && statusCode >= 500 {
		e.ala.Alarm(err)
	}

	var m EndpointMeta
	if len(meta) > 0 {
		m = meta[0]
	}
	for _, mw := range e.middlewares[OnErrorResponse] {
		mw(ctx, m) //nolint:errcheck
	}
	for _, mw := range e.localMdw[OnErrorResponse] {
		mw(ctx, m) //nolint:errcheck
	}
}

// compile validates the endpoint configuration and precomputes all reflection
// data needed at request time. Called once per endpoint during BuildHandlers.
func (e *Endpoint) compile(v *Vatel) error {
	origPath := e.Path
	e.Path = path.Join(v.cfg.urlPrefix, e.Path)

	if err := compileCommon(v, e); err != nil {
		return err
	}

	if e.ResponseContentType != "" {
		e.responseContentType = []byte(e.ResponseContentType)
	} else {
		e.responseContentType = []byte("application/json; charset=utf-8")
	}

	c := e.Controller()

	// Validate path param / Paramer consistency.
	re := regexp.MustCompile(`(?s)\{(.*)\}`)
	pathHasParam := re.Match([]byte(e.Path))
	_, isParamer := c.(Paramer)

	if !isParamer && pathHasParam {
		return fmt.Errorf("endpoint %s %s: path has parameters but controller does not implement Paramer", e.Method, origPath)
	}
	if isParamer && !pathHasParam {
		return fmt.Errorf("endpoint %s %s: controller implements Paramer but path has no parameters", e.Method, origPath)
	}

	e.isPathParametrized = isParamer
	if isParamer {
		e.paramFields = buildParamFields(reflect.TypeOf(c.(Paramer).Param()).Elem())
	}

	_, hasRespBody := c.(Resulter)
	e.hasRespBody = hasRespBody
	e.compileMask(c)

	ii, isInputer := c.(Inputer)

	// Querier input is decoded from the URL query string for every method.
	qi, isQuerier := c.(Querier)
	e.isQuerier = isQuerier
	if isQuerier {
		e.querierFields = buildQueryFields(reflect.TypeOf(qi.QueryInput()).Elem())
		e.queryMaskFields = e.jm.Fields(qi.QueryInput(), "mask")
	}

	// Route input decoding based on HTTP method.
	switch e.Method {
	case "GET", "DELETE", "HEAD", "OPTIONS":
		if e.OptionalBody {
			return fmt.Errorf("endpoint %s %s: WithOptionalBody is not allowed for a method without a request body", e.Method, origPath)
		}
		e.isURLQueryExpected = isInputer
		if isInputer {
			e.queryFields = buildQueryFields(reflect.TypeOf(ii.Input()).Elem())
		}
	case "POST", "PUT", "PATCH":
		e.isRequestBodyExpected = isInputer
	default:
		return fmt.Errorf("endpoint %s has unknown HTTP method", origPath)
	}

	return nil
}

// resultPrototyper is implemented by OptionalOutput, whose Result() is nil
// until the handler sets it: it returns a typed nil pointer to the result
// type, so masking rules can be built at compile time.
type resultPrototyper interface {
	resultPrototype() interface{}
}

// compileMask precomputes masking rules of the Inputer input and the Resulter
// result of controller c. A result type unknown at compile time (Result()
// returns an untyped nil) gets its rules built per response.
func (e *Endpoint) compileMask(c interface{}) {
	if ri, ok := c.(Resulter); ok {
		res := ri.Result()
		if res == nil {
			if p, ok := c.(resultPrototyper); ok {
				res = p.resultPrototype()
			}
		}
		e.resultFieldsDynamic = res == nil
		e.resultFields = e.jm.Fields(res, "mask")
	}
	if ii, ok := c.(Inputer); ok {
		e.inputFields = e.jm.Fields(ii.Input(), "mask")
	}
}

// defaultMasker masks log output of endpoints when WithJsonMasker is not set.
var defaultMasker = jsonmask.New()

// compileCommon copies shared Vatel config into the endpoint.
// Auth fields (auth, perms) are left to compileWS for WS endpoints, or to
// per-endpoint middleware (vatel/auth) for HTTP endpoints.
func compileCommon(v *Vatel, e *Endpoint) error {
	e.middlewares = v.mdw
	e.staticLoggingLevel = v.cfg.staticLoggingLevel
	e.verboseError = v.cfg.verboseError
	e.logRequestID = v.cfg.logRequestID
	e.clientErrorFields = v.cfg.clientErrorFields
	e.jm = v.cfg.jm
	if e.jm == nil {
		e.jm = defaultMasker
	}
	e.ala = v.cfg.ala
	e.mr = v.cfg.mr

	if e.LogOptions == LogUnknown {
		e.LogOptions = v.cfg.defaultLogOption
	}

	return nil
}
