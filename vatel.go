package vatel

import (
	"context"
	stderrors "errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/axkit/vatel/jsonmask"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

// AuthPayload is the application part of a decoded token, returned by
// Tokener.ApplicationPayload. The vatel core does not call its methods; they
// are used by vatel/auth: Perms for the permission check of RequirePerms and
// Debug to turn on verbose error responses for the request.
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
	Fields(structure any, tag string) jsonmask.Fields
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
	built bool
	// buildErr is the error of a failed BuildHandlers. The router may hold the
	// routes registered before the failure, so the Vatel is unusable after it.
	buildErr error

	// baseCtx is the parent of every request context; Shutdown cancels it.
	// fasthttp's RequestCtx.Done() is not used: it is the server's done
	// channel, closed only by Server.Shutdown.
	baseCtx context.Context
	cancel  context.CancelFunc

	mu      sync.Mutex       // guards the fields below
	srv     *fasthttp.Server // server started by ListenAndServe
	ln      net.Listener     // its listener
	closed  bool             // Shutdown was called
	stopped chan struct{}    // closed when srv has stopped; set by Shutdown
}

// DefaultIdleTimeout is the IdleTimeout of the server started by
// ListenAndServe: a keep-alive connection without a new request is closed
// after it. Change it with WithServerConfig.
const DefaultIdleTimeout = 90 * time.Second

// shutdownGrace is how long Shutdown waits, after it has cancelled the
// request contexts, for the cancelled requests to finish.
const shutdownGrace = time.Second

// ErrServerClosed is returned by ListenAndServe called after Shutdown.
var ErrServerClosed = stderrors.New("vatel: server closed")

var (
	errHandlersBuilt = stderrors.New("vatel: handlers already built")
	errServerStarted = stderrors.New("vatel: server already started")
)

// NewVatel returns a new Vatel instance. The root GET / endpoint (table of
// contents) is registered automatically.
func NewVatel(optFunc ...func(*Option)) *Vatel {
	v := Vatel{}
	for i := range optFunc {
		optFunc[i](&v.cfg)
	}
	v.baseCtx, v.cancel = context.WithCancel(context.Background())
	v.ep = []Endpoint{{EndpointMeta: EndpointMeta{Method: fasthttp.MethodGet, Path: "/"}, Controller: func() Handler { return &tocController{s: &v} }}}
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
	serverConfig       func(*fasthttp.Server)
}

// WithServerConfig sets a function that configures the fasthttp.Server
// started by ListenAndServe, e.g. its ReadTimeout or MaxRequestBodySize. It is
// called after vatel has set Handler, IdleTimeout (DefaultIdleTimeout) and
// CloseOnShutdown (true). Do not replace Handler. Hooks such as ConnState are
// free for the application: Shutdown does not use them.
func WithServerConfig(f func(*fasthttp.Server)) func(*Option) {
	return func(o *Option) { o.serverConfig = f }
}

// WithMetricReporter sets the reporter of per-request metrics: method, path
// template, status, duration and response size (0 for HEAD).
func WithMetricReporter(mr MetricReporter) func(*Option) {
	return func(o *Option) { o.mr = mr }
}

// WithWebsocketSupport enables WS endpoints, served by wv. The application
// routes an HTTP path to wv.Upgrade.
func WithWebsocketSupport(wv *WebsocketVatel) func(*Option) {
	return func(o *Option) { o.wv = wv }
}

// WithUrlPrefix sets a prefix joined to the path of every HTTP endpoint, e.g.
// "/api/v1".
func WithUrlPrefix(s string) func(*Option) {
	return func(o *Option) { o.urlPrefix = s }
}

// WithRequestID logs the fasthttp request ID as "reqId" (connectionId for WS
// messages); it is also stored in the request context under "reqId".
func WithRequestID() func(*Option) {
	return func(o *Option) { o.logRequestID = true }
}

// WithStaticLoggingLevel makes vatel ignore the X-Set-Log-Level request
// header.
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

// WithDefaultLogOption sets the log verbosity of endpoints without their own
// WithLogOption. Without it only errors are logged.
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

// WithAlarmer sets the receiver of errors with status >= 500.
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
	if err := v.BuildHandlers(mux, l); err != nil {
		panic(err.Error())
	}
}

// BuildHandlers registers all endpoints with the router. It returns an error
// instead of panicking: an invalid endpoint (e.g. a Param(), QueryInput() or
// GET Input() that does not return a pointer to a struct), a path that does
// not begin with "/", a route the router rejects (a duplicate or conflicting path, or an own
// GET "/", which is taken by the built-in endpoint list), a WS endpoint
// without WithWebsocketSupport or a duplicate WS path.
//
// Middleware (AddMiddleware) and endpoints (Add) must be added before
// BuildHandlers: it copies them, later additions have no effect.
//
// BuildHandlers may be called only once. Registration is not transactional:
// after a failed build the router may already hold some of the routes and
// they cannot be removed, so the Vatel is unusable. A further BuildHandlers
// or ListenAndServe returns a "previous build failed" error; create a new
// Vatel and router instead.
func (v *Vatel) BuildHandlers(mux *router.Router, l *zerolog.Logger) error {
	if v.built {
		if v.buildErr != nil {
			return fmt.Errorf("vatel: previous build failed: %w", v.buildErr)
		}
		return errHandlersBuilt
	}
	v.built = true
	v.buildErr = v.registerEndpoints(mux, l)
	return v.buildErr
}

func (v *Vatel) registerEndpoints(mux *router.Router, l *zerolog.Logger) error {

	if v.cfg.wv != nil {
		v.cfg.wv.bind(v, l)
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

		if e.Method == methodWS {
			if v.cfg.wv == nil {
				return fmt.Errorf("endpoint %s %s: websocket support is not set (WithWebsocketSupport)", methodWS, e.Path)
			}
			if err := v.cfg.wv.RegisterEndpoint(v, e, &logger); err != nil {
				return err
			}
			continue
		}

		if err := e.compile(v); err != nil {
			return err
		}
		h := e.handler(&logger)
		if e.Compress {
			h = fasthttp.CompressHandler(h)
		}
		if err := registerRoute(mux, e.Method, e.Path, h); err != nil {
			return err
		}

		logger.Info().Msg("handler registered")
	}

	return nil
}

// registerRoute registers h with the router, returning the router's panic (a
// duplicate or conflicting route) as an error.
func registerRoute(mux *router.Router, method, path string, h fasthttp.RequestHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("endpoint %s %s: %v", method, path, r)
		}
	}()
	mux.Handle(method, path, h)
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
// before calling this method. The server is configured with
// DefaultIdleTimeout and CloseOnShutdown; see WithServerConfig.
//
// Use Shutdown to stop the server gracefully; ListenAndServe then returns
// nil. Called after Shutdown, it returns ErrServerClosed without starting.
func (v *Vatel) ListenAndServe(addr string, mux *router.Router, l *zerolog.Logger) error {
	if !v.built || v.buildErr != nil {
		if err := v.BuildHandlers(mux, l); err != nil {
			return err
		}
	}
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		return err
	}
	return v.serve(ln, mux)
}

// serve serves mux on ln until Shutdown.
func (v *Vatel) serve(ln net.Listener, mux *router.Router) error {
	srv := &fasthttp.Server{
		Handler:         mux.Handler,
		IdleTimeout:     DefaultIdleTimeout,
		CloseOnShutdown: true,
	}
	if v.cfg.serverConfig != nil {
		v.cfg.serverConfig(srv)
	}
	sl := &serverListener{Listener: ln}

	v.mu.Lock()
	if v.closed || v.srv != nil {
		started := v.srv != nil
		v.mu.Unlock()
		ln.Close() //nolint:errcheck
		if started {
			return errServerStarted
		}
		return ErrServerClosed
	}
	v.srv, v.ln = srv, sl
	v.mu.Unlock()

	err := srv.Serve(sl)

	v.mu.Lock()
	closed := v.closed
	v.mu.Unlock()
	if closed {
		// Shutdown may close the listener before Serve registers it; the
		// accept error is then the expected end of serving.
		return nil
	}
	return err
}

// serverListener is the listener of the server started by ListenAndServe.
// Close is idempotent, so both Shutdown and fasthttp's ShutdownWithContext
// can close it.
type serverListener struct {
	net.Listener
	once sync.Once
	err  error
}

func (l *serverListener) Close() error {
	l.once.Do(func() { l.err = l.Listener.Close() })
	return l.err
}

// Shutdown gracefully stops the server:
//
//  1. It closes all active WebSocket connections without waiting for them
//     (clients receive a "going away" frame and can reconnect; see
//     WebsocketVatel.Shutdown) and stops accepting new connections.
//     ListenAndServe returns nil; called later, it returns ErrServerClosed.
//  2. It waits until no HTTP request is in flight: requests being handled
//     and requests still being received (headers or body being read) finish
//     normally, and their responses carry "Connection: close". Idle
//     keep-alive connections do not hold it up: they are closed. A new
//     connection that has sent nothing yet gets about 5 seconds to start its
//     request (as in net/http), then it is closed too.
//  3. It cancels the request contexts (Context.Context()) and returns nil.
//
// When ctx is done before step 2 ends, the request contexts of requests still
// in flight are cancelled at once, so handlers listening to them can stop
// (a context.Canceled error gives 503). Shutdown then waits up to one more
// second for them to finish and returns ctx.Err(); requests still running
// after that are not interrupted, and their responses carry
// "Connection: close".
//
// Without a server started by ListenAndServe the request contexts are
// cancelled at once. Requests that start after Shutdown get a cancelled
// context. Calling Shutdown again, also concurrently, is safe: each call
// waits for the server with its own ctx.
//
// Pass a context with a deadline to bound the graceful phase:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	defer cancel()
//	v.Shutdown(ctx)
func (v *Vatel) Shutdown(ctx context.Context) error {
	v.mu.Lock()
	v.closed = true
	srv := v.srv
	v.mu.Unlock()

	if v.cfg.wv != nil {
		v.cfg.wv.Shutdown()
	}
	if srv == nil {
		v.cancel()
		return nil
	}

	stopped := v.stopServer()
	select {
	case <-stopped:
		v.cancel()
		return nil
	case <-ctx.Done():
	}

	v.cancel()
	t := time.NewTimer(shutdownGrace)
	defer t.Stop()
	select {
	case <-stopped:
	case <-t.C:
	}
	return ctx.Err()
}

// stopServer starts stopping the server started by ListenAndServe, once, and
// returns a channel closed when it has stopped.
//
// fasthttp's ShutdownWithContext closes the listeners and the idle
// connections, makes responses carry "Connection: close" (CloseOnShutdown)
// and waits until no connection is open. It runs without a deadline: when
// its context expires it resets the server's stop flag and forgets the
// listeners, so the connections still open would go on serving requests and
// a second call would not wait for them. Shutdown applies its own ctx while
// waiting for the returned channel.
func (v *Vatel) stopServer() <-chan struct{} {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopped == nil {
		v.stopped = make(chan struct{})
		srv, ln, stopped := v.srv, v.ln, v.stopped
		// ShutdownWithContext does nothing when it runs before Serve has
		// registered the listener, so the listener is closed here as well.
		ln.Close() //nolint:errcheck
		go func() {
			srv.ShutdownWithContext(context.Background()) //nolint:errcheck
			close(stopped)
		}()
	}
	return v.stopped
}
