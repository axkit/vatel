package vatel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/axkit/date"
	"github.com/axkit/errors"
	"github.com/axkit/vatel/jsonmask"
	"github.com/regorov/websocket"
	"github.com/rs/zerolog"

	//	goon "github.com/shurcooL/go-goon"
	//	"github.com/hexops/valast"
	realip "github.com/ferluci/fast-realip"
	"github.com/valyala/fasthttp"
)

type LogOption uint32

const (
	LogSilent LogOption = 1 << iota
	LogEnter
	LogExit
	LogReqBody
	LogReqInput
	LogRespBody
	LogRespOutput
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

type MiddlewarePos int

const (
	BeforeAuthorization MiddlewarePos = iota
	AfterAuthorization
	OnSuccessResponse
	OnErrorResponse
)

type middlewareSet [3][]func(Context) error

// Endpoint describes a REST endpoint attributes and related request Handler.
type Endpoint struct {
	staticLoggingLevel bool
	verboseError       bool
	logRequestID       bool
	LogOptions         LogOption

	// Method holds HTTP method name (e.g GET, POST, PUT, DELETE, WS).
	Method string

	// Wraps response by gzip compression function.
	Compress bool

	// Path holds url path with fasthttp parameters (e.g. /customers/{id}).
	Path string

	// Perms holds list of permissions. Nil if endpoint is public.
	Perms []string

	// Controller holds reference to the object implementing interface Handler.
	Controller func() Handler

	WebsocketController func() WebsocketHandler

	// ResponseContentType by default has "application/json; charset: utf-8;"
	ResponseContentType string
	responseContentType []byte

	// NoInputLog defines debug logging rule for request data. If true, endpoint request body
	// will not be written to the log. (i.e authentication endpoint).
	NoInputLog bool

	// NoResultLog defines debug logging rule for response data. If true, endpoint response body
	// will not be written to the log. (i.e authentication  endpoint)
	NoResultLog bool

	ManualStatusCode bool

	//
	SuccessStatusCode int

	isPathParametrized    bool
	isURLQueryExpected    bool
	isRequestBodyExpected bool
	hasRespBody           bool

	LanguageLabel string
	auth          Authorizer
	td            TokenDecoder
	pm            PermissionManager
	rd            RequestDebugger
	rtc           RevokeTokenChecker
	perms         []uint

	middlewares middlewareSet

	jm           JsonMasker
	inputFields  jsonmask.Fields
	resultFields jsonmask.Fields

	ala      Alarmer
	mr       MetricReporter
	wsLogger *zerolog.Logger
}

// NewEndpoint builds Endpoint.
func NewEndpoint(method, path string, perms []string, c func() Handler) *Endpoint {
	return &Endpoint{Method: method, Path: path, Perms: append([]string{}, perms...), Controller: c}
}

// Endpointer is the interface that wraps a single Endpoints method.
//
// Endpoints returns []Endpoints to be handled by API gateway.
type Endpointer interface {
	Endpoints() []Endpoint
}

// Handler is the interface what wraps Handle method.
//
// Handle invocates by API gateway mux.
type Handler interface {
	Handle(Context) error
}

// WebsocketHandler is the interface what wraps Handle method.
//
// Handle invocates by websocket message processing func.
type WebsocketHandler interface {
	Handle(WebsocketContext) error
}

// Inputer is the interface what wraps Input method.
//
// Input returns reference to the object what will be promoted
// with input data by vatel.
//
// If endpoint's handler expects input data, Input method should be
// implemented.
//
// GET, DELETE methods:  input values will be taken from URL query.
// POST, PUT, PATCH methods: input values will be taken from JSON body.
type Inputer interface {
	Input() interface{}
}

// Resulter is the interface what wraps a single Result method.
//
// Result returns reference to the object what will be
// send to the client when endpoint handler completes successfully.
//
// If endpoint's controller have outgoing data, Result method should be implemented.
type Resulter interface {
	Result() interface{}
}

// Paramer is the interface what wraps a single Param method.
//
// Param returns reference to the struct what will be promoted with
// values from URL.
//
// Example: if we have /customer/{id}/bill/{billnum} then
// Param() should return reference to struct
//
//	{
//			CustomerID int `param:"id"
//		 	BillNum string `param:"billnum"`
//	}
//
// If there is URL params and variables like /customer/{id}?sortBy=name&balanceAbove=100
// methods Param and Input can return reference to the same struct.
type Paramer interface {
	Param() interface{}
}

func (e *Endpoint) writeErrorResponse(ctx Context, verbose bool, zc *zerolog.Context, err error) {
	if err == nil {
		return
	}

	fmt.Printf("err: %#v\n", err)
	statusCode := 500
	ce, ok := err.(*errors.CatchedError)

	fmt.Printf("ce: %#v\n", ce)
	if ok {
		statusCode = ce.Last().StatusCode
		if statusCode == 429 {
			// in case of too many requests, look if error has attribute Retry-After
			var hv []byte
			if ra, ok := ce.Get("Retry-After"); ok {
				switch ra.(type) {
				case int, int64, int32, int16, int8, uint, uint64, uint32, uint16, uint8:
					hv = []byte(fmt.Sprintf("%d", ra))
				case string:
					hv = []byte(ra.(string))
				case []byte:
					hv = ra.([]byte)
				}
				ctx.SetHeader([]byte("Retry-After"), hv)
			}
		}
	}

	z := *zc
	ctx.VisitUserValues(func(key []byte, v interface{}) {
		z = z.Interface(string(key), v)
	})

	zl := z.RawJSON("err", errors.ToServerJSON(err)).Logger()
	zl.Error().Msg("request failed")

	ctx.SetContentType([]byte("application/json; charset=utf-8"))
	ctx.SetStatusCode(statusCode)

	var ff errors.FormattingFlag
	if verbose {
		ff = errors.AddStack | errors.AddFields | errors.AddWrappedErrors
	}

	_, xerr := ctx.BodyWriter().Write(errors.ToJSON(err, ff))

	if xerr != nil {
		//zl.With().Error().RawJSON("err", errors.ToServerJSON(xerr)).Msg("writing http response failed")
	}

	if e.mr != nil {
		e.mr.ReportMetric(e.Method, e.Path, statusCode, time.Since(ctx.RequestCtx().Time()).Seconds(), len(ctx.RequestCtx().Response.Body()))
	}

	if e.ala != nil && statusCode >= 500 {
		e.ala.Alarm(err)
	}

	return
}

func (e *Endpoint) handler(l *zerolog.Logger) func(*fasthttp.RequestCtx) {

	return func(fctx *fasthttp.RequestCtx) {

		var (
			zc  zerolog.Context
			zco zerolog.Context
		)

		verbose := e.verboseError

		var lo LogOption
		if !e.staticLoggingLevel {
			logLevels := fctx.Request.Header.Peek("X-Set-Log-Level")
			if len(logLevels) > 0 {
				lvls := strings.Split(string(logLevels), ",")
				var n uint32 = 0
				for _, lvl := range lvls {
					if v, ok := logOptionString[strings.TrimSpace(lvl)]; ok {
						n |= uint32(v)
					}
				}
				atomic.StoreUint32((*uint32)(&e.LogOptions), n)
				fctx.Write([]byte(fmt.Sprintf("new logging rules are accepted: %b", n)))
				return
			} else {
				lo = LogOption(atomic.LoadUint32((*uint32)(&e.LogOptions)))
			}
		} else {
			lo = e.LogOptions
		}

		zco = l.With().Str("client", realip.FromRequest(fctx))
		if e.logRequestID {
			zco = zco.Uint64("reqId", fctx.ID())
		}
		zc = zco

		ctx := NewContext(fctx)

		for i := range e.middlewares[BeforeAuthorization] {
			if err := e.middlewares[BeforeAuthorization][i](ctx); err != nil {
				e.writeErrorResponse(ctx, verbose, &zc, err)
				return
			}
		}

		// inDebug := e.LogOptions&ConfidentialInput != ConfidentialInput
		// outDebug := e.LogOptions&ConfidentialOutput != ConfidentialOutput

		at := fctx.Request.Header.Peek("Authorization") // access token

		if e.auth != nil {

			switch len(e.Perms) {
			case 0:
				break
			case 1:
				zc = zc.Str("perm", e.Perms[0])
			default:
				zc = zc.Strs("perms", e.Perms)
			}

			if len(at) == 0 && len(e.Perms) > 0 {
				e.writeErrorResponse(ctx, verbose, &zc, ErrAuthorizationHeaderMissed.Capture())
				return
			}

			if len(at) > 0 {
				token, err := e.authorize(at)
				if err != nil {
					e.writeErrorResponse(ctx, verbose, &zc, err)
					return
				}

				if e.rd != nil {
					//	inDebug, outDebug = e.rd.IsDebugRequired(token.ApplicationPayload())
				}
				t := token.ApplicationPayload()
				ctx.SetTokenPayload(t)
				verbose = verbose || t.Debug()
			}

		}
		// if e.auth != nil {
		// 	if len(e.Perms) > 0 {
		// 		switch len(e.Perms) {
		// 		case 0:
		// 			break
		// 		case 1:
		// 			zc = zc.Str("perm", e.Perms[0])
		// 		default:
		// 			zc = zc.Strs("perms", e.Perms)
		// 		}

		// 		if len(at) == 0 {
		// 			e.writeErrorResponse(ctx, verbose, &zc, ErrAuthorizationHeaderMissed.Capture())
		// 			return
		// 		}

		// 		token, err := e.authorize(at)
		// 		if err != nil {
		// 			e.writeErrorResponse(ctx, verbose, &zc, err)
		// 			return
		// 		}

		// 		if e.rd != nil {
		// 			//	inDebug, outDebug = e.rd.IsDebugRequired(token.ApplicationPayload())
		// 		}
		// 		t := token.ApplicationPayload()
		// 		ctx.SetTokenPayload(t)
		// 		verbose = verbose || t.Debug()
		// 	}
		// }

		if fctx.QueryArgs().GetBool("description") {
			if err := e.handleDescription(ctx); err != nil {
				e.writeErrorResponse(ctx, verbose, &zc, err)
			}
			return
		}

		zc, h, err := e.initController(fctx, lo, zc)
		if err != nil {
			e.writeErrorResponse(ctx, verbose, &zc, err)
			return
		}

		for i := range e.middlewares[AfterAuthorization] {
			if err := e.middlewares[AfterAuthorization][i](ctx); err != nil {
				e.writeErrorResponse(ctx, verbose, &zc, err)
				return
			}
		}

		if lo&LogEnter == LogEnter {
			ctx.RequestCtx().VisitUserValues(func(key []byte, v interface{}) {
				zc = zc.Interface(string(key), v)
			})

			zl := zc.Logger()
			zl.Debug().Msg("new request")
			zc = zco
		}

		if err = h.Handle(ctx); err != nil {
			e.writeErrorResponse(ctx, verbose, &zc, err)
			return
		}

		if e.hasRespBody {
			if err := e.writeResponse(ctx, lo, h.(Resulter).Result(), &zc); err != nil {
				e.writeErrorResponse(ctx, verbose, &zc, err)
				return
			}
		}

		dur := time.Since(fctx.Time())
		if lo&LogExit == LogExit {
			msg := "completed"
			if e.LogOptions&LogEnter != LogEnter {
				msg = "processed"
			}
			ctx.VisitUserValues(func(key []byte, v interface{}) {
				if bytes.Equal(key, []byte("message")) {
					msg = v.(string)
					return
				}
				zc = zc.Interface(string(key), v)
			})

			zl := zc.Logger()
			zl.Debug().Str("dur", dur.String()).Msg(msg)
		}

		if e.mr != nil {
			e.mr.ReportMetric(e.Method, e.Path, 200, dur.Seconds(), len(fctx.Response.Body()))
		}

		for i := range e.middlewares[OnSuccessResponse] {
			if err := e.middlewares[OnSuccessResponse][i](ctx); err != nil {
				e.writeErrorResponse(ctx, verbose, &zc, err)
				return
			}
		}
	}
}

func (e *Endpoint) writeResponse(ctx Context, lo LogOption, res interface{}, zc *zerolog.Context) error {

	buf, err := json.Marshal(res)
	if err != nil {
		*zc = zc.Interface("result", res)
		return err
	}

	if lo&LogRespOutput == LogRespOutput {
		*zc = zc.Interface("result", res)
	}

	ctx.SetContentType(e.responseContentType)

	if lo&LogRespBody != LogRespBody {
		_, err = ctx.BodyWriter().Write(buf)
		return err
	}

	if e.jm != nil && len(e.resultFields) > 0 {
		maskedBuf, err := e.jm.Mask(buf, e.resultFields)
		if err != nil {
			maskedBuf = []byte(`{"maskingError": "` + err.Error() + `"}`)
		}

		*zc = zc.RawJSON("maskedRespBody", maskedBuf)
	} else {
		*zc = zc.RawJSON("respBody", buf)
	}

	_, err = ctx.BodyWriter().Write(buf)
	return err
}

func (e *Endpoint) wsWriteResponse(c *websocket.Conn, lo LogOption, res interface{}, zc *zerolog.Context) error {

	buf, err := json.Marshal(res)
	if err != nil {
		*zc = zc.Interface("result", res)
		return err
	}

	if lo&LogRespOutput == LogRespOutput {
		*zc = zc.Interface("result", res)
	}

	if lo&LogRespBody != LogRespBody {
		_, err = c.Write(buf)
		return err
	}

	if e.jm != nil && len(e.resultFields) > 0 {
		maskedBuf, err := e.jm.Mask(buf, e.resultFields)
		if err != nil {
			maskedBuf = []byte(`{"maskingError": "` + err.Error() + `"}`)
		}

		*zc = zc.RawJSON("maskedRespBody", maskedBuf)
	} else {
		*zc = zc.RawJSON("respBody", buf)
	}

	_, err = c.Write(buf)
	return err
}

var (
	ErrAuthorizationHeaderMissed = errors.New("header Authorization missed").Code("VTL-0001").StatusCode(401).Critical()
	ErrAccessTokenRevoked        = errors.New("access token revoked").Code("VTL-0002").StatusCode(401).Critical()
)

func (e *Endpoint) authorize(at []byte) (Tokener, error) {

	if e.rtc != nil {
		isRevoked, err := e.rtc.IsTokenRevoked(string(at))
		if err != nil {
			return nil, err
		}

		if isRevoked {
			return nil, ErrAccessTokenRevoked.Capture()
		}
	}

	token, err := e.td.Decode(at)
	if err != nil {
		return nil, errors.Catch(err).SetStrs("perms", e.Perms...).Msg("unauthorized")
	}

	if len(e.Perms) == 0 {
		return token, nil
	}

	isAllowed, err := e.auth.IsAllowed(token.ApplicationPayload().Perms(), e.perms...)
	if err == nil {
		if isAllowed {
			return token, nil
		}
		return nil, errors.Forbidden().
			Set("user", token.ApplicationPayload().Login()).
			Set("role", token.ApplicationPayload().Role()).
			SetStrs("perms", e.Perms...)
	}

	return nil, errors.Catch(err).
		Set("user", token.ApplicationPayload().Login()).
		Set("role", token.ApplicationPayload().Role()).
		SetStrs("perms", e.Perms...).
		StatusCode(401)

}

func (e *Endpoint) initController(ctx *fasthttp.RequestCtx, lo LogOption, zc zerolog.Context) (zerolog.Context, Handler, error) {

	var (
		err error
		h   = e.Controller()
	)

	if e.isPathParametrized {
		p := h.(Paramer).Param()
		if zc, err = decodeParams(ctx, p, zc); err != nil {
			return zc, nil, err
		}
	}

	if e.isURLQueryExpected {
		in := h.(Inputer).Input()
		if zc, err = decodeURLQuery(ctx, in, zc); err != nil {
			return zc, nil, err
		}
	}

	if e.isRequestBodyExpected {
		if lo&LogReqBody == LogReqBody {
			var (
				cJSON *bytes.Buffer // compacted json
				buf   []byte
			)

			cJSON = bytes.NewBuffer(nil)
			key := "requestBody"
			err := json.Compact(cJSON, ctx.Request.Body())
			if err != nil {
				zc = zc.Str("requestBodyLoggingFailed", err.Error())
			} else {
				buf = cJSON.Bytes()
				if e.jm != nil && len(e.inputFields) > 0 {
					if buf, err = e.jm.Mask(cJSON.Bytes(), e.inputFields); err == nil {
						key = "maskedRequestBody"
					} else {
						zc = zc.Str("maskingRequestAttrsFailed", err.Error())
					}
				}
				if err == nil {
					zc = zc.RawJSON(key, buf)
				}
			}
		}

		in := h.(Inputer).Input()
		if err := decodeBody(ctx, in); err != nil {
			return zc, nil, err
		}
		if lo&LogReqInput == LogReqInput {
			zc = zc.Interface("reqInput", in)
		}
	}

	return zc, h, nil
}

// Doc возвращает описание входных и выходных параметров контроллера.
func (e *Endpoint) handleDescription(ctx Context) error {

	c := e.Controller()

	ctx.SetContentType([]byte("text/html; charset=utf-8"))

	_, err := ctx.BodyWriter().Write(e.genDescription(c))
	if err != nil {
		return errors.Catch(err).StatusCode(500).Msg("description response write failed")
	}
	return nil
}

func (e *Endpoint) genDescription(c Handler) []byte {
	s := "Endpoint description: " + e.Method + " -  " + e.Path
	if c == nil {
		s += "No handler"
	}

	if e.isPathParametrized {
		//s += "\n" + goon.SDump(c.(Paramer).Param())
		//s += "\n" + valast.String(c.(Paramer).Param()) + "\n"
	}

	if e.isRequestBodyExpected {
		//s += "Body input: \n" + valast.String(c.(Inputer).Input())
	}

	if e.isURLQueryExpected {
		//s += "URL input\n" + valast.String(c.(Inputer).Input())
	}

	if e.hasRespBody {
		//s += "\n" + valast.String(c.(Resulter).Result())
	}

	return []byte(s)
}

// TODO: сделать поддержку param не в виде структуры, а в виде одной переменной.
func decodeParams(ctx *fasthttp.RequestCtx, param interface{}, zcin zerolog.Context) (zerolog.Context, error) {

	zc := zcin
	s := reflect.ValueOf(param).Elem()
	tof := s.Type()

	for i := 0; i < tof.NumField(); i++ {
		sf := s.Field(i)

		if sf.CanSet() == false {
			continue
		}

		tag := tof.Field(i).Tag.Get("param")
		if tag == "" {
			continue
		}

		uv := ctx.UserValue(tag)
		if uv == nil {
			return zc, errors.InternalError().Set("name", tag).Msg("path param not found")
		}

		val, ok := uv.(string)
		if !ok {
			panic("non string param")
		}

		zc = zc.Interface(tag, val)

		switch sf.Interface().(type) {
		case int, int8, int16, int32, int64:
			i, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return zc, errors.ValidationFailed(err.Error())
			}
			sf.SetInt(i)
		case uint, uint8, uint16, uint32, uint64:
			i, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				return zc, errors.ValidationFailed(err.Error())
			}
			sf.SetUint(i)
		case string:
			sf.SetString(val)
		case bool:
			b, err := strconv.ParseBool(val)
			if err != nil {
				return zc, errors.ValidationFailed(err.Error())
			}
			sf.SetBool(b)
		case float32, float64:
			f, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return zc, errors.ValidationFailed(err.Error())
			}
			sf.SetFloat(f)
		case []string:
			break
		default:
			return zc, errors.ValidationFailed("unsupported go type").Set("tag", tag)
		}
	}
	return zc, nil
}

func assign(val string, i interface{}) error {
	return nil
}

func decodeBody(ctx *fasthttp.RequestCtx, dest interface{}) error {
	buf := ctx.Request.Body()
	if len(buf) == 0 {
		return errors.InvalidRequestBody("empty request body. JSON expected")
	}
	return json.Unmarshal(buf, dest)
}

func decodeURLQuery(ctx *fasthttp.RequestCtx, input interface{}, zc zerolog.Context) (zerolog.Context, error) {

	s := reflect.ValueOf(input).Elem()
	tof := s.Type()

	for i := 0; i < tof.NumField(); i++ {
		sf := s.Field(i)
		atof := tof.Field(i)

		if sf.CanSet() == false {
			continue
		}

		if sf.Kind() == reflect.Struct {
			if zc, err := decodeURLQuery(ctx, sf.Addr().Interface(), zc); err != nil {
				return zc, err
			}
			continue
		}

		tag := atof.Tag.Get("param")
		if tag == "" {
			continue
		}

		val := ctx.QueryArgs().Peek(tag)
		zc = zc.Bytes(tag, val)
		if val == nil {
			continue
		}

		if sf.Kind() == reflect.Ptr {
			if sf.IsNil() {
				sf.Set(reflect.New(sf.Type().Elem()))
			}
			sf = sf.Elem()
		}

		if atof.Type.Name() == "Date" {
			if _, ok := sf.Interface().(date.Date); ok {
				d, err := date.Parse(string(val))
				if err != nil {
					return zc, err
				}
				sf.SetUint(uint64(d))
			}
			continue
		}

		switch sf.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			k, err := strconv.ParseInt(string(val), 10, 64)
			if err != nil {
				return zc, err
			}
			sf.SetInt(k)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			k, err := strconv.ParseUint(string(val), 10, 64)
			if err != nil {
				return zc, err
			}
			sf.SetUint(k)
		case reflect.String:
			sf.SetString(string(val))
		case reflect.Float64:
			k, err := strconv.ParseFloat(string(val), 64)
			if err != nil {
				return zc, err
			}
			sf.SetFloat(k)
		case reflect.Float32:
			k, err := strconv.ParseFloat(string(val), 32)
			if err != nil {
				return zc, err
			}
			sf.SetFloat(k)
		case reflect.Bool:
			b, err := strconv.ParseBool(string(val))
			if err != nil {
				return zc, err
			}
			sf.SetBool(b)
		default:
			return zc, errors.ValidationFailed("unsupported type").Set("val", string(val)).Set("kind", sf.Kind().String())
		}
	}
	return zc, nil
}

func (e *Endpoint) compile(v *Vatel) error {
	opath := e.Path
	e.Path = path.Join(v.cfg.urlPrefix, e.Path)
	e.auth = v.auth
	e.td = v.td
	e.pm = v.pm
	e.rd = v.rd
	e.rtc = v.rtc
	e.middlewares = v.mdw
	e.staticLoggingLevel = v.cfg.staticLoggingLevel
	e.verboseError = v.cfg.verboseError
	e.logRequestID = v.cfg.logRequestID
	e.jm = v.cfg.jm
	e.ala = v.cfg.ala
	e.mr = v.cfg.mr

	if e.LogOptions == LogUnknown {
		e.LogOptions = v.cfg.defaultLogOption
	}

	if e.LogOptions&LogSilent == e.LogOptions {
		e.LogOptions = e.LogOptions
	}

	if e.ResponseContentType != "" {
		e.responseContentType = []byte(e.ResponseContentType)
	} else {
		e.responseContentType = []byte("application/json; charset=utf-8")
	}

	if len(e.Perms) > 0 {
		if e.auth == nil && !v.authDisabled {
			return fmt.Errorf("endpoint %s %s requires calling SetAuthorizer() before", e.Method, opath)
		}
		if e.td == nil && !v.authDisabled {
			return fmt.Errorf("endpoint %s %s requires calling SetTokenDecode() before", e.Method, opath)
		}

		if e.pm == nil && !v.authDisabled {
			return fmt.Errorf("endpoint %s %s requires calling SetPermissionManager() before", e.Method, opath)
		}

		for i := range e.Perms {
			pb, ok := v.pm.PermissionBitPos(e.Perms[i])
			if !ok {
				return fmt.Errorf("endpoint %s %s mentioned unknown permission %s", e.Method, opath, e.Perms[i])
			}
			e.perms = append(e.perms, pb)
		}
	}
	c := e.Controller()

	// looking for "{ }"" in the path
	re, err := regexp.Compile(`(?s)\{(.*)\}`)
	if err != nil {
		return fmt.Errorf("endpoint %s %s cannot be parsed by regexp", e.Method, opath)
	}

	_, isParamer := c.(Paramer)
	pathHasParam := re.Match([]byte(e.Path))

	if !isParamer && pathHasParam {
		return fmt.Errorf("endpoint %s %s path has parameters, but controller does not implement Paramer", e.Method, opath)
	}

	if isParamer && !pathHasParam {
		return fmt.Errorf("endpoint %s %s path has no parameters, but controller implement Paramer", e.Method, opath)
	}
	e.isPathParametrized = isParamer

	ri, hasRespBody := c.(Resulter)
	if hasRespBody && e.jm != nil {
		e.resultFields = e.jm.Fields(ri.Result(), "mask")
	}
	e.hasRespBody = hasRespBody

	ii, isInputer := c.(Inputer)
	if isInputer && e.jm != nil {
		e.inputFields = e.jm.Fields(ii.Input(), "mask")
	}

	switch e.Method {
	case "GET", "DELETE":
		e.isURLQueryExpected = isInputer
	case "POST", "PUT", "PATCH":
		e.isRequestBodyExpected = isInputer
	default:
		return fmt.Errorf("endpoint %s has unknown HTTP method %s", opath, e.Method)
	}
	return nil
}
