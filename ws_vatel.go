package vatel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/axkit/errors"
	"github.com/regorov/websocket"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

// connStore is a thread-safe map from WebSocket connection ID to its token payload.
// It persists authentication state across messages on the same connection.
type connStore struct {
	mu    sync.RWMutex
	conns map[uint64]AuthPayload
}

// wsConnRegistry is a thread-safe map from connection ID to the live Conn,
// used to enumerate and close all connections during shutdown.
type wsConnRegistry struct {
	mu    sync.RWMutex
	conns map[uint64]*websocket.Conn
}

func (r *wsConnRegistry) add(c *websocket.Conn) {
	r.mu.Lock()
	r.conns[c.ID()] = c
	r.mu.Unlock()
}

func (r *wsConnRegistry) remove(cid uint64) {
	r.mu.Lock()
	delete(r.conns, cid)
	r.mu.Unlock()
}

func (r *wsConnRegistry) closeAll() {
	r.mu.RLock()
	conns := make([]*websocket.Conn, 0, len(r.conns))
	for _, c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.RUnlock()
	for _, c := range conns {
		c.CloseDetail(websocket.StatusGoAway, "server shutdown")
	}
}

func (cs *connStore) set(cid uint64, tp AuthPayload) {
	cs.mu.Lock()
	cs.conns[cid] = tp
	cs.mu.Unlock()
}

func (cs *connStore) get(cid uint64) (AuthPayload, bool) {
	cs.mu.RLock()
	tp, ok := cs.conns[cid]
	cs.mu.RUnlock()
	return tp, ok
}

func (cs *connStore) delete(cid uint64) {
	cs.mu.Lock()
	delete(cs.conns, cid)
	cs.mu.Unlock()
}

// WebsocketVatel manages WebSocket endpoint registration and message routing.
type WebsocketVatel struct {
	isPublicAccessAllowed bool
	ws                    websocket.Server
	path                  map[string]*Endpoint
	cfg                   WsOption
	store                 connStore
	registry              wsConnRegistry
	callbackOnOpen        func(*websocket.Conn)
	callbackOnClose       func(*websocket.Conn, error)
	callbackOnTokenUpdate func(cid uint64, tp AuthPayload) error
}

// WsOption holds WebSocket-specific configuration.
type WsOption struct {
	log      *zerolog.Logger
	td       TokenDecoder
	authFunc func(ctx WebsocketContext, path string, requiredPerms []string) error
}

func WithLogger(log *zerolog.Logger) func(*WsOption) {
	return func(o *WsOption) {
		zl := log.With().Str("layer", "ws").Logger()
		o.log = &zl
	}
}

func WithTokenDecoder(td TokenDecoder) func(*WsOption) {
	return func(o *WsOption) { o.td = td }
}

// WithWsAuthFunc sets the per-message authorization function. It is called for
// every incoming message before the handler runs. ctx carries the token payload
// (set via UpdateAccessToken or a prior message), path is the endpoint path,
// and requiredPerms is the slice declared on the endpoint (may be empty).
// Return a non-nil error to reject the message.
func WithWsAuthFunc(fn func(ctx WebsocketContext, path string, requiredPerms []string) error) func(*WsOption) {
	return func(o *WsOption) { o.authFunc = fn }
}

// NewWebsocketVatel creates a WebsocketVatel and registers the built-in
// updateAccessToken endpoint.
func NewWebsocketVatel(optFunc ...func(*WsOption)) *WebsocketVatel {
	wv := &WebsocketVatel{
		path:     make(map[string]*Endpoint),
		store:    connStore{conns: make(map[uint64]AuthPayload)},
		registry: wsConnRegistry{conns: make(map[uint64]*websocket.Conn)},
	}
	for _, f := range optFunc {
		f(&wv.cfg)
	}
	wv.ws.HandleData(wv.onMessage)
	// Always register onOpen and onClose so the registry stays consistent.
	wv.ws.HandleOpen(wv.onOpen)
	wv.ws.HandleClose(wv.onClose)
	return wv
}

// Endpoints returns the built-in WebSocket endpoints exposed by WebsocketVatel.
func (wv *WebsocketVatel) Endpoints() []Endpoint {
	return []Endpoint{
		{
			EndpointMeta:        EndpointMeta{Path: "updateAccessToken", Method: "WS", LogOptions: LogConfidential},
			WebsocketController: func() WebsocketHandler { return &AccessTokenUpdateHandler{wv: wv} },
		},
	}
}

// RegisterEndpoint is called by Vatel.buildHandlers for every endpoint with method "WS".
func (wv *WebsocketVatel) RegisterEndpoint(v *Vatel, e *Endpoint, l *zerolog.Logger) error {
	if _, ok := wv.path[e.Path]; ok {
		return errors.Template("websocket endpoint already registered").New().Set("path", e.Path)
	}
	if err := wv.compileWS(v, e, l); err != nil {
		return err
	}
	wv.path[e.Path] = e
	return nil
}

// compileWS validates and configures a WS endpoint, reusing compileCommon
// for the shared Vatel config injection.
func (wv *WebsocketVatel) compileWS(v *Vatel, e *Endpoint, l *zerolog.Logger) error {
	if err := compileCommon(v, e); err != nil {
		return err
	}
	e.wsLogger = l

	// If the endpoint declares required permissions, an auth function must be set.
	if len(e.Perms) > 0 && wv.cfg.authFunc == nil {
		return fmt.Errorf("WS endpoint %s: WithWsAuthFunc() must be set for endpoints declaring Perms", e.Path)
	}

	c := e.WebsocketController()
	if c == nil {
		return fmt.Errorf("endpoint WS %s: missing WebsocketController", e.Path)
	}

	_, hasRespBody := c.(Resulter)
	e.hasRespBody = hasRespBody
	e.compileMask(c)

	_, isInputer := c.(Inputer)
	e.isRequestBodyExpected = isInputer

	return nil
}

// OnOpen registers a callback invoked when a new WebSocket connection is established.
func (wv *WebsocketVatel) OnOpen(f func(*websocket.Conn)) { wv.callbackOnOpen = f }

// OnClose registers a callback invoked when a WebSocket connection closes.
func (wv *WebsocketVatel) OnClose(f func(*websocket.Conn, error)) { wv.callbackOnClose = f }

func (wv *WebsocketVatel) onOpen(c *websocket.Conn) {
	wv.registry.add(c)
	if wv.callbackOnOpen != nil {
		wv.callbackOnOpen(c)
	}
}

func (wv *WebsocketVatel) onClose(c *websocket.Conn, err error) {
	wv.registry.remove(c.ID())
	wv.store.delete(c.ID())
	if wv.callbackOnClose != nil {
		wv.callbackOnClose(c, err)
	}
}

// Shutdown closes all active WebSocket connections with a "going away" status.
// Call this before shutting down the HTTP server to allow clients to reconnect.
func (wv *WebsocketVatel) Shutdown() {
	wv.registry.closeAll()
}

// Upgrade upgrades an HTTP connection to WebSocket.
func (wv *WebsocketVatel) Upgrade(ctx *fasthttp.RequestCtx) {
	wv.ws.Upgrade(ctx)
}

// UpgradeWithID upgrades an HTTP connection to WebSocket with a caller-supplied ID.
func (wv *WebsocketVatel) UpgradeWithID(ctx *fasthttp.RequestCtx, id uint64) {
	wv.ws.UpgradeWithID(ctx, id)
}

// WebsocketRequest is the envelope for all incoming WebSocket messages.
type WebsocketRequest struct {
	Path string          `json:"path"`
	Data json.RawMessage `json:"data,omitempty"`
}

// AuthMessageData is the payload for token-based authentication messages.
type AuthMessageData struct {
	AccessToken string `json:"accessToken"`
}

// ToClientJSON serialises an error for transmission to the WebSocket client.
func ToClientJSON(err error) []byte {
	if err == nil {
		return nil
	}
	return errors.ToJSON(err, errors.WithAttributes(errors.ClientOutputFormat))
}

func (wv *WebsocketVatel) onMessage(c *websocket.Conn, isBinary bool, data []byte) {
	ctx := NewWsContext(c)

	var msg WebsocketRequest
	if err := json.Unmarshal(data, &msg); err != nil {
		c.Write(ToClientJSON(ErrInvalidRequestBody.Wrap(err).StatusCode(400).Msg("bad request"))) //nolint:errcheck
		return
	}

	if msg.Path == "" {
		c.Write(ToClientJSON(errors.Template("bad request").New().StatusCode(400).Set("reason", "empty path"))) //nolint:errcheck
		return
	}

	e, ok := wv.path[msg.Path]
	if !ok {
		c.Write(ToClientJSON(errors.Template("unknown path").New().StatusCode(404))) //nolint:errcheck
		return
	}

	verbose := e.verboseError

	var lo LogOption
	if e.staticLoggingLevel {
		lo = e.LogOptions
	} else {
		lo = LogOption(atomic.LoadUint32((*uint32)(&e.LogOptions)))
	}

	zco := e.wsLogger.With().Str("client", c.RemoteAddr().String())
	if e.logRequestID {
		zco = zco.Uint64("connectionId", c.ID())
	}
	zc := zco

	// Restore token payload from the connection store (set on auth or token update).
	if tp, ok := wv.store.get(c.ID()); ok {
		ctx.SetTokenPayload(tp)
	}

	// ── Authorization ────────────────────────────────────────────────────────
	if wv.cfg.authFunc != nil {
		if len(e.Perms) > 0 {
			switch len(e.Perms) {
			case 1:
				zc = zc.Str("perm", e.Perms[0])
			default:
				zc = zc.Strs("perms", e.Perms)
			}
		}
		if err := wv.cfg.authFunc(ctx, msg.Path, e.Perms); err != nil {
			wsWriteErrorResponse(e, c, verbose, &zc, ctx.Created(), err)
			return
		}
	}

	if lo&LogEnter == LogEnter {
		zl := zc.Logger()
		zl.Debug().Msg("new ws request")
		zc = zco
	}

	h := e.WebsocketController()

	// ── Input decoding ───────────────────────────────────────────────────────
	if input, ok := h.(Inputer); ok {
		if err := json.Unmarshal(msg.Data, input.Input()); err != nil {
			wsWriteErrorResponse(e, c, verbose, &zc, ctx.Created(), err)
			return
		}
	}

	// ── Handler ──────────────────────────────────────────────────────────────
	if err := h.Handle(ctx); err != nil {
		wsWriteErrorResponse(e, c, verbose, &zc, ctx.Created(), err)
		return
	}

	// ── Response serialization ───────────────────────────────────────────────
	if e.hasRespBody {
		res := h.(Resulter).Result()
		if !isNilResult(res) {
			if err := e.wsWriteResponse(c, lo, res, &zc); err != nil {
				wsWriteErrorResponse(e, c, verbose, &zc, ctx.Created(), err)
				return
			}
		}
	}

	// ── Exit logging + metrics ───────────────────────────────────────────────
	dur := time.Since(ctx.Created())
	if lo&LogExit == LogExit {
		msg := "completed"
		if lo&LogEnter != LogEnter {
			msg = "processed"
		}
		ctx.VisitValues(func(key []byte, v interface{}) {
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
		e.mr.ReportMetric(e.Method, e.Path, 200, dur.Seconds(), 0)
	}
}

// wsWriteErrorResponse logs an error and sends it to the WebSocket client.
func wsWriteErrorResponse(e *Endpoint, c *websocket.Conn, verbose bool, zc *zerolog.Context, created time.Time, err error) {
	if err == nil {
		return
	}

	statusCode := 500
	if xe, ok := err.(*errors.Error); ok {
		ce := errors.Serialize(xe, errors.WithAttributes(errors.ServerOutputFormat))
		statusCode = ce.StatusCode
	}

	z := *zc
	zl := z.RawJSON("err", errors.ToJSON(err, errors.WithAttributes(errors.ServerOutputFormat))).Logger()
	zl.Error().Msg("ws request failed")

	var ff errors.ErrorSerializationRule
	if verbose {
		ff = errors.AddStack | errors.AddFields | errors.AddWrappedErrors
	}
	var rootFields []string
	if e != nil {
		rootFields = e.clientErrorFields
	}
	buf := errors.ToJSON(err, errors.WithAttributes(ff), errors.WithRootLevelFields(rootFields))
	c.Write(buf) //nolint:errcheck

	if e != nil && e.mr != nil {
		e.mr.ReportMetric(e.Method, e.Path, statusCode, time.Since(created).Seconds(), len(buf))
	}

	if e != nil && e.ala != nil && statusCode >= 500 {
		e.ala.Alarm(err)
	}
}

// UpdateAccessToken decodes a new access token, stores it in the connection
// store, and notifies any registered callback.
func (wv *WebsocketVatel) UpdateAccessToken(cid uint64, token string) error {
	if wv.cfg.td == nil {
		return nil
	}
	at, err := wv.cfg.td.Decode([]byte(token))
	if err != nil {
		return err
	}
	tp := at.ApplicationPayload()
	wv.store.set(cid, tp)
	if wv.callbackOnTokenUpdate != nil {
		wv.callbackOnTokenUpdate(cid, tp)
	}
	return nil
}
