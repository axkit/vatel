package vatel

import (
	"sort"
	"strings"

	"github.com/axkit/vatel/jsonmask"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

// TokenPayloader is the interface that wraps access methods
// to JWT payload parts.
//
// User returns value of user attribute from the token.
//
// Perms returns bitset array with user role's permissions.
type TokenPayloader interface {
	User() int
	Login() string
	Role() int
	Perms() []byte
	Extra() interface{}
	Debug() bool
}

// Tokener is the interface that wraps methods SystemPayload and UserPayload.
//
// SystemPayload returns JWT part related to JWT itself.
//
// UserPayload returns an object that represents JWT payload specified by user.
type Tokener interface {
	SystemPayload() map[string]interface{}
	ApplicationPayload() TokenPayloader
}

// Authorizer is the interface that wraps IsAllowed method.
//
// Authorizer accepts request permissions and permissions required by endpoint.
// Returns true if all endpointPerms are inside requestPerms.
type Authorizer interface {
	IsAllowed(requestPerms []byte, endpointPerms ...uint) (bool, error)
}

type RequestDebugger interface {
	IsDebugRequired(TokenPayloader) (in, out bool)
}

// PermissionManager ...
type PermissionManager interface {
	PermissionBitPos(perm string) (uint, bool)
}

// RevokeTokenChecker is the interface what wraps a single method IsTokenRevoked.
//
// IsTokenRevoked returns true if access token was revoked.
type RevokeTokenChecker interface {
	IsTokenRevoked(accessToken string) (bool, error)
}

// TokenDecoder is the interface what wraps a single method Decode.
//
// TokenDecoder decodes token and returns object Tokener.
type TokenDecoder interface {
	Decode(encodedToken []byte) (Tokener, error)
}

// MetricReporter is the interface what wraps a single method ReportMetric.
//
// HTTP requests handler uses MetricReporter to submit processing metrics to prometheus.
type MetricReporter interface {
	ReportMetric(method, path string, statusCode int, dur float64, size int)
}

type Logger interface {
	Log()
}

// Vatel holds
type Vatel struct {
	ep   []Endpoint
	auth Authorizer
	td   TokenDecoder
	pm   PermissionManager
	rd   RequestDebugger
	rtc  RevokeTokenChecker

	mdw middlewareSet

	authDisabled bool
	cfg          Option
	ws           *WebsocketGateway
}

// NewVatel returns new instance of Vatel.
func NewVatel(optFunc ...func(*Option)) *Vatel {

	v := Vatel{}

	for i := range optFunc {
		optFunc[i](&v.cfg)
	}

	v.ep = []Endpoint{{Method: "GET", Path: "/", Controller: func() Handler { return &tocController{s: &v} }}}
	return &v
}
func (va *Vatel) AddWebsocketSupport(dws *WebsocketGateway) {
	va.ws = dws
}

type Option struct {
	urlPrefix          string
	staticLoggingLevel bool
	defaultLogOption   LogOption
	verboseError       bool
	logRequestID       bool
	jm                 JsonMasker
	ala                Alarmer
	mr                 MetricReporter
	// websocketUpgradePath string
	// ws                   *websocket.Server
	// wsStore              WebSocketStorer
}

func WithMetricReporter(mr MetricReporter) func(*Option) {
	return func(o *Option) {
		o.mr = mr
	}
}

func WithUrlPrefix(s string) func(*Option) {
	return func(o *Option) {
		o.urlPrefix = s
	}
}

func WithRequestID() func(*Option) {
	return func(o *Option) {
		o.logRequestID = true
	}
}

func WithStaticLoggingLevel() func(*Option) {
	return func(o *Option) {
		o.staticLoggingLevel = true
	}
}

// func WithWebsocket(upgradePath string, wss WebSocketStorer) func(*Option) {
// 	return func(o *Option) {
// 		o.websocketUpgradePath = upgradePath
// 		o.wsStore = wss
// 	}
// }

// WithVerboseError sets verbose mode for error response to the client.
func WithVerboseError(b bool) func(*Option) {
	return func(o *Option) {
		o.verboseError = b
	}
}

func WithDefaultLogOption(lo LogOption) func(*Option) {
	return func(o *Option) {
		o.defaultLogOption = lo
	}
}

func WithJsonMasker(jm JsonMasker) func(*Option) {
	return func(o *Option) {
		o.jm = jm
	}
}

func WithAlarmer(ala Alarmer) func(*Option) {
	return func(o *Option) {
		o.ala = ala
	}
}

// SetAuthorizer assigns authorization implementation.
// If Authorizer is not assigned, all Endpoint's Perms will be ignored.
func (v *Vatel) SetAuthorizer(a Authorizer) {
	v.auth = a
}

// SetRevokeTokenChecker assigns implementation of access token
// validation in the storage of revoked access tokens.
func (v *Vatel) SetRevokeTokenChecker(rtc RevokeTokenChecker) {
	v.rtc = rtc
}

func (v *Vatel) DisableAuthorizer() {
	v.authDisabled = true
}

// SetPermissionManager assigns implementation of permission manager.
//
func (v *Vatel) SetPermissionManager(pm PermissionManager) {
	v.pm = pm
}

// SetRequestDebugger assigns request debugger implementation.
func (v *Vatel) SetRequestDebugger(rd RequestDebugger) {
	v.rd = rd
}

// SetTokenDecoder assigns session token decoder.
func (v *Vatel) SetTokenDecoder(tp TokenDecoder) {
	v.td = tp
}

// Add add endpoints to the list.
//
// The method does not check Endpoint for corectness and uqiqueness here.
// Paths validation implemented by method BuildHandlers.
func (v *Vatel) Add(e ...Endpointer) {
	for i := range e {
		v.ep = append(v.ep, e[i].Endpoints()...)
	}
}

// Endpoints returns all registered endpoints.
func (v *Vatel) Endpoints() []Endpoint {
	return v.ep
}

// MustBuildHandlers initializes http mux with rules by converting []Endpoint
// added before. Panics if:
// 	- there are Perms but SetAuthorizer or SetTokenDecoder were not called.
// 	-
func (v *Vatel) MustBuildHandlers(mux *router.Router, l *zerolog.Logger) {
	if err := v.buildHandlers(mux, l); err != nil {
		panic(err.Error())
	}
}

// BuildHandlers initializes http mux with rules by converting []Endpoint
// added before. Returns errors instead of panic.
func (v *Vatel) BuildHandlers(mux *router.Router, l *zerolog.Logger) error {
	return v.buildHandlers(mux, l)
}

func (v *Vatel) buildHandlers(mux *router.Router, l *zerolog.Logger) error {

	if v.ws != nil {
		v.ep = append(v.ep, v.ws.Endpoints()...)
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

	// if v.cfg.websocketUpgradePath != "" {
	// 	// v.ws.OnMessage =
	// 	if v.cfg.wsStore != nil {
	// 		v.ws.HandleOpen(v.cfg.wsStore.OnOpen)
	// 		v.ws.HandleClose(v.cfg.wsStore.OnClose)
	// 		v.ep = append(v.ep, Endpoint{
	// 			LogOptions: LogExit,
	// 			Method:     "WS",
	// 			Path:       "auth",
	// 			Controller: func() Handler { return &AuthConnectionHandler{v: v} },
	// 		})
	// 	}
	// 	v.wsPath = make(map[string]*Endpoint)
	// 	for i := range v.ep {
	// 		e := &v.ep[i]
	// 		if e.Method != "WS" { // Regular HTTP handlers processed above.
	// 			continue
	// 		}
	// 		if _, ok := v.wsPath[e.Path]; ok {
	// 			panic(e.Path + " is already registered")
	// 		}
	// 		logger := l.With().Str("method", e.Method).Str("path", e.Path).Logger()
	// 		if err := e.wsCompile(v, &logger); err != nil {
	// 			return err
	// 		}
	// 		v.wsPath[e.Path] = e

	// 		// if e.Compress {
	// 		// 	mux.Handle(e.Method, e.Path, fasthttp.CompressHandler(e.handler(&logger)))
	// 		// } else {
	// 		// 	mux.Handle(e.Method, e.Path, e.handler(&logger))
	// 		// }
	// 		logger.Info().Msg("ws handler registered")
	// 	}
	// 	v.ep = append(v.ep, Endpoint{
	// 		LogOptions: LogExit,
	// 		Method:     "GET",
	// 		Path:       v.cfg.websocketUpgradePath,
	// 		Controller: func() Handler { return &UpgradeConnectionHandler{v: v} },
	// 	})

	// 	v.ws.HandleData(v.wsMessageHandler)
	// }

	for i := range v.ep {
		e := &v.ep[i]
		logger := l.With().Str("method", e.Method).Str("path", e.Path).Logger()

		if e.Method == "WS" { // websocket processed externally.
			if v.ws == nil {
				panic("websocket server was not assigned")
			}
			if err := v.ws.RegisterEndpoint(v, e, &logger); err != nil {
				panic(err)
			}
			continue
		} else {
			if err := e.compile(v); err != nil {
				return err
			}
			if e.Compress {
				mux.Handle(e.Method, e.Path, fasthttp.CompressHandler(e.handler(&logger)))
			} else {
				mux.Handle(e.Method, e.Path, e.handler(&logger))
			}
		}

		logger.Info().Msg("handler registered")
	}

	return nil
}

type Dater interface {
	Parse(string) (interface{}, error)
	Set(interface{})
}

// AddMiddleware adds middlewares to be called for every requests in
// the same order.
func (v *Vatel) AddMiddleware(pos MiddlewarePos, f ...func(Context) error) {
	v.mdw[pos] = append(v.mdw[pos], f...)
}

// JsonMasker is the interface that wraps Mask and Fields methods.
//
// Fields returns meta data of the struct which is receiver of coming JSON data.
// The metadata is used further in the method Mask.
//
// Mask modifies JSON using rules specified for each field structure.
// Returns JSON with masked sensitive values.
type JsonMasker interface {
	Fields(structure interface{}, tag string) jsonmask.Fields
	Mask(src []byte, fields jsonmask.Fields) ([]byte, error)
}

// Alarmer is the interface that wraps a single method Alarm.
type Alarmer interface {
	Alarm(err error)
}
