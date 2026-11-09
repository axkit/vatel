package vatel

import (
	"context"
	stderrors "errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	built bool
	// buildErr is the error of a failed BuildHandlers. The router may hold the
	// routes registered before the failure, so the Vatel is unusable after it.
	buildErr error

	// baseCtx is the parent of every request context; Shutdown cancels it.
	// fasthttp's RequestCtx.Done() is not used: it is the server's done
	// channel, closed only by Server.Shutdown.
	baseCtx context.Context
	cancel  context.CancelFunc

	// inflight counts HTTP requests being processed by vatel endpoints.
	inflight atomic.Int64

	mu     sync.Mutex       // guards the fields below
	srv    *fasthttp.Server // server started by ListenAndServe
	ln     net.Listener     // its listener
	served chan struct{}    // closed when the server stops accepting connections
	closed bool             // Shutdown was called
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
	serverConfig       func(*fasthttp.Server)
}

// WithServerConfig sets a function that configures the fasthttp.Server
// started by ListenAndServe, e.g. its ReadTimeout or MaxRequestBodySize. It is
// called after vatel has set Handler, IdleTimeout (DefaultIdleTimeout) and
// CloseOnShutdown (true). Do not replace Handler.
func WithServerConfig(f func(*fasthttp.Server)) func(*Option) {
	return func(o *Option) { o.serverConfig = f }
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
		return fmt.Errorf("vatel: handlers already built")
	}
	v.built = true
	v.buildErr = v.registerEndpoints(mux, l)
	return v.buildErr
}

func (v *Vatel) registerEndpoints(mux *router.Router, l *zerolog.Logger) error {

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
				return fmt.Errorf("WS endpoint %s: websocket support is not set (WithWebsocketSupport)", e.Path)
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
		if err := handle(mux, e.Method, e.Path, h); err != nil {
			return err
		}

		logger.Info().Msg("handler registered")
	}

	return nil
}

// handle registers h with the router, returning the router's panic (a
// duplicate or conflicting route) as an error.
func handle(mux *router.Router, method, path string, h fasthttp.RequestHandler) (err error) {
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
	sl := &serverListener{Listener: ln, srv: srv}

	v.mu.Lock()
	if v.closed || v.srv != nil {
		started := v.srv != nil
		v.mu.Unlock()
		ln.Close() //nolint:errcheck
		if started {
			return fmt.Errorf("vatel: server already started")
		}
		return ErrServerClosed
	}
	served := make(chan struct{})
	v.srv, v.ln, v.served = srv, sl, served
	v.mu.Unlock()

	err := srv.Serve(sl)
	close(served)

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
// Close is idempotent, so both Shutdown and fasthttp's Server.Shutdown can
// close it. Accepted TCP connections get the keep-alive settings of the
// server, as fasthttp's own ListenAndServe does.
type serverListener struct {
	net.Listener
	srv  *fasthttp.Server
	once sync.Once
	err  error
}

func (l *serverListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok && l.srv.TCPKeepalive {
		tc.SetKeepAlive(true) //nolint:errcheck
		if p := l.srv.TCPKeepalivePeriod; p > 0 {
			tc.SetKeepAlivePeriod(p) //nolint:errcheck
		}
	}
	return c, nil
}

func (l *serverListener) Close() error {
	l.once.Do(func() { l.err = l.Listener.Close() })
	return l.err
}

// Shutdown gracefully stops the server:
//
//  1. It closes all active WebSocket connections (clients receive a "going
//     away" frame and can reconnect) and stops accepting new connections.
//     ListenAndServe returns nil; called later, it returns ErrServerClosed.
//  2. It waits until no HTTP request is in flight. Idle keep-alive
//     connections do not hold it up; the server closes them when they send
//     a request (the response carries "Connection: close") or after the
//     IdleTimeout.
//  3. It cancels the request contexts (Context.Context()) and returns nil.
//
// When ctx is done before step 2 ends, the request contexts of requests still
// in flight are cancelled at once, so handlers listening to them can stop
// (a context.Canceled error gives 503). Shutdown then waits up to one more
// second for them to finish and returns ctx.Err().
//
// Without a server started by ListenAndServe the request contexts are
// cancelled at once. Requests that start after Shutdown get a cancelled
// context. Calling Shutdown again is safe.
//
// Pass a context with a deadline to bound the graceful phase:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	defer cancel()
//	v.Shutdown(ctx)
func (v *Vatel) Shutdown(ctx context.Context) error {
	v.mu.Lock()
	v.closed = true
	srv, ln, served := v.srv, v.ln, v.served
	v.mu.Unlock()

	if v.cfg.wv != nil {
		v.cfg.wv.Shutdown()
	}
	if srv == nil {
		v.cancel()
		return nil
	}

	// fasthttp's Shutdown does nothing when it runs before Serve has
	// registered the listener, so the listener is closed here as well.
	ln.Close() //nolint:errcheck
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Shutdown() }()

	done, err := v.waitRequests(ctx, served, srvDone)
	v.cancel()
	if done {
		return err
	}

	grace, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	v.waitRequests(grace, served, srvDone) //nolint:errcheck
	return ctx.Err()
}

// waitRequests waits until the server has stopped accepting connections and
// no request is in flight, or until srvDone delivers the result of the
// server's Shutdown (all connections are closed). done is false when ctx is
// done first.
func (v *Vatel) waitRequests(ctx context.Context, served <-chan struct{}, srvDone <-chan error) (done bool, err error) {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-served:
			if v.inflight.Load() == 0 {
				return true, nil
			}
		default:
		}
		select {
		case err := <-srvDone:
			return true, err
		case <-ctx.Done():
			return false, ctx.Err()
		case <-t.C:
		}
	}
}
