package vatel

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/axkit/errors"
	"github.com/axkit/vatel/jsonmask"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

// AuthPayload is the minimal interface the framework requires from a decoded
// token. It carries only what vatel needs internally: permission bytes for the
// authorization check and a debug flag for verbose error responses.
//
// Handlers retrieve the payload via ctx.TokenPayload() and type-assert it to
// their concrete type (e.g. ctx.TokenPayload().(MyJWTClaims)).
type AuthPayload interface {
	Perms() []byte
	Debug() bool
}

// Tokener wraps the system and application parts of a decoded JWT.
type Tokener interface {
	SystemPayload() map[string]any
	ApplicationPayload() AuthPayload
}

// RevokeTokenChecker reports whether an access token has been revoked.
type RevokeTokenChecker interface {
	IsTokenRevoked(accessToken string) (bool, error)
}

// TokenDecoder decodes a raw token into a Tokener.
type TokenDecoder interface {
	Decode(encodedToken []byte) (Tokener, error)
}

// MetricReporter submits per-request metrics (e.g. to Prometheus).
type MetricReporter interface {
	ReportMetric(method, path string, statusCode int, dur float64, size int)
}

// JsonMasker masks sensitive fields in JSON payloads for logging.
type JsonMasker interface {
	Fields(structure interface{}, tag string) jsonmask.Fields
	Mask(src []byte, fields jsonmask.Fields) ([]byte, error)
}

// Alarmer receives critical errors for alerting.
type Alarmer interface {
	Alarm(err error)
}

// Vatel is the central API gateway. It registers endpoints, configures shared
// services, and builds fasthttp handlers.
type Vatel struct {
	ep    []Endpoint
	mdw   middlewareSet
	cfg   Option
	srv   *fasthttp.Server
	built bool

	// baseCtx is the parent of every request context; Shutdown cancels it.
	// fasthttp's RequestCtx.Done() is not used: it is the server's done
	// channel, closed only by Server.Shutdown.
	baseCtx context.Context
	cancel  context.CancelFunc
}

// NewVatel returns a new Vatel instance. The root GET / endpoint (table of
// contents) is registered automatically.
func NewVatel(optFunc ...func(*Option)) *Vatel {
	v := Vatel{}
	for i := range optFunc {
		optFunc[i](&v.cfg)
	}
	v.baseCtx, v.cancel = context.WithCancel(context.Background())
	v.ep = []Endpoint{{EndpointMeta: EndpointMeta{Method: "GET", Path: "/"}, Controller: func() Handler { return &tocController{s: &v} }}}
	return &v
}

// Option holds configuration for a Vatel instance.
type Option struct {
	urlPrefix          string
	staticLoggingLevel bool
	defaultLogOption   LogOption
	verboseError       bool
	logRequestID       bool
	clientErrorFields  []string
	defaultTimeout     time.Duration
	jm                 JsonMasker
	ala                Alarmer
	mr                 MetricReporter
	wv                 *WebsocketVatel
}

func WithMetricReporter(mr MetricReporter) func(*Option) {
	return func(o *Option) { o.mr = mr }
}

func WithWebsocketSupport(wv *WebsocketVatel) func(*Option) {
	return func(o *Option) { o.wv = wv }
}

func WithUrlPrefix(s string) func(*Option) {
	return func(o *Option) { o.urlPrefix = s }
}

func WithRequestID() func(*Option) {
	return func(o *Option) { o.logRequestID = true }
}

func WithStaticLoggingLevel() func(*Option) {
	return func(o *Option) { o.staticLoggingLevel = true }
}

// WithVerboseError enables verbose error responses (stack traces, wrapped errors).
func WithVerboseError(b bool) func(*Option) {
	return func(o *Option) { o.verboseError = b }
}

// WithClientErrorFields lists error fields (set via errors.Set) that are safe
// to show to the client. They are written at the root of the error JSON even
// when verbose errors are off; all other fields stay in the server log only.
//
//	vatel.WithClientErrorFields("reason", "suggestedName")
func WithClientErrorFields(fields ...string) func(*Option) {
	return func(o *Option) { o.clientErrorFields = fields }
}

// WithDefaultTimeout sets the request context timeout of endpoints without
// their own WithTimeout. See WithTimeout.
func WithDefaultTimeout(d time.Duration) func(*Option) {
	return func(o *Option) { o.defaultTimeout = d }
}

func WithDefaultLogOption(lo LogOption) func(*Option) {
	return func(o *Option) { o.defaultLogOption = lo }
}

// WithJsonMasker sets the masker applied to logged request and response
// bodies, inputs and results of types with `mask` struct tags. Without it a
// default jsonmask.New() is used, so tagged fields are always masked in the
// log ("***" for a function name not registered with AddFunc).
func WithJsonMasker(jm JsonMasker) func(*Option) {
	return func(o *Option) { o.jm = jm }
}

func WithAlarmer(ala Alarmer) func(*Option) {
	return func(o *Option) { o.ala = ala }
}

// Add appends endpoints to the registry. Validation happens in BuildHandlers.
func (v *Vatel) Add(e ...Endpointer) {
	for i := range e {
		v.ep = append(v.ep, e[i].Endpoints()...)
	}
}

// Endpoints returns all registered endpoints.
func (v *Vatel) Endpoints() []Endpoint { return v.ep }

// MustBuildHandlers registers all endpoints with the router. Panics on error.
func (v *Vatel) MustBuildHandlers(mux *router.Router, l *zerolog.Logger) {
	if err := v.buildHandlers(mux, l); err != nil {
		panic(err.Error())
	}
}

// BuildHandlers registers all endpoints with the router. Returns errors instead of panicking.
func (v *Vatel) BuildHandlers(mux *router.Router, l *zerolog.Logger) error {
	return v.buildHandlers(mux, l)
}

func (v *Vatel) buildHandlers(mux *router.Router, l *zerolog.Logger) error {
	if v.built {
		return fmt.Errorf("vatel: handlers already built")
	}
	v.built = true

	if v.cfg.wv != nil {
		v.ep = append(v.ep, v.cfg.wv.Endpoints()...)
	}

	for i := range v.ep {
		v.ep[i].Method = strings.ToUpper(v.ep[i].Method)
	}

	sort.Slice(v.ep, func(i, j int) bool {
		if v.ep[i].Path == v.ep[j].Path {
			return v.ep[i].Method < v.ep[j].Method
		}
		return v.ep[i].Path < v.ep[j].Path
	})

	for i := range v.ep {
		e := &v.ep[i]
		logger := l.With().Str("method", e.Method).Str("path", e.Path).Logger()

		if e.Method == "WS" {
			if v.cfg.wv == nil {
				panic("websocket server was not assigned")
			}
			if err := v.cfg.wv.RegisterEndpoint(v, e, &logger); err != nil {
				panic(string(errors.ToJSON(err, errors.WithAttributes(errors.ServerDebugOutputFormat))))
			}
			continue
		}

		if err := e.compile(v); err != nil {
			return err
		}
		if e.Compress {
			mux.Handle(e.Method, e.Path, fasthttp.CompressHandler(e.handler(&logger)))
		} else {
			mux.Handle(e.Method, e.Path, e.handler(&logger))
		}

		logger.Info().Msg("handler registered")
	}

	return nil
}

// AddMiddleware registers middleware at the given pipeline position.
// Middleware functions are called in the order they are added.
func (v *Vatel) AddMiddleware(pos MiddlewarePos, f ...func(Context, EndpointMeta) error) {
	v.mdw[pos] = append(v.mdw[pos], f...)
}

// ListenAndServe compiles all endpoints (unless BuildHandlers was already
// called), starts the HTTP server on addr, and blocks until the server stops.
// The caller retains ownership of mux so that non-vatel routes can be added
// before calling this method.
//
// Use Shutdown to stop the server gracefully.
func (v *Vatel) ListenAndServe(addr string, mux *router.Router, l *zerolog.Logger) error {
	if !v.built {
		if err := v.buildHandlers(mux, l); err != nil {
			return err
		}
	}
	v.srv = &fasthttp.Server{Handler: mux.Handler}
	return v.srv.ListenAndServe(addr)
}

// Shutdown gracefully stops the server. It first closes all active WebSocket
// connections (clients receive a "going away" frame and can reconnect), then
// waits for in-flight HTTP requests to finish or until ctx is cancelled.
// Finally it cancels the request contexts (Context.Context()) of requests
// still in flight, so handlers listening to them can stop. Without a server
// started by ListenAndServe the request contexts are cancelled at once.
// Calling Shutdown again is safe.
//
// Pass a context with a deadline to enforce a hard cutoff:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	defer cancel()
//	v.Shutdown(ctx)
func (v *Vatel) Shutdown(ctx context.Context) error {
	if v.cancel != nil {
		defer v.cancel()
	}
	if v.cfg.wv != nil {
		v.cfg.wv.Shutdown()
	}
	if v.srv == nil {
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- v.srv.Shutdown() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
