package vatel

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/axkit/errors"
	"github.com/regorov/websocket"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

// wsConns is the thread-safe table of live WebSocket connections: the
// connection, to close it on shutdown, and its token payload, which persists
// authentication state across messages on the same connection.
type wsConns struct {
	mu    sync.RWMutex
	conns map[uint64]*wsConn
}

type wsConn struct {
	conn *websocket.Conn // nil when a payload is set for an unknown connection
	tp   AuthPayload
}

func newWsConns() wsConns {
	return wsConns{conns: make(map[uint64]*wsConn)}
}

func (cs *wsConns) add(c *websocket.Conn) {
	cs.mu.Lock()
	if wc, ok := cs.conns[c.ID()]; ok {
		wc.conn = c
	} else {
		cs.conns[c.ID()] = &wsConn{conn: c}
	}
	cs.mu.Unlock()
}

// setPayload stores the token payload of the connection cid.
func (cs *wsConns) setPayload(cid uint64, tp AuthPayload) {
	cs.mu.Lock()
	if wc, ok := cs.conns[cid]; ok {
		wc.tp = tp
	} else {
		cs.conns[cid] = &wsConn{tp: tp}
	}
	cs.mu.Unlock()
}

// payload returns the token payload of the connection cid, if one is stored.
func (cs *wsConns) payload(cid uint64) (AuthPayload, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if wc, ok := cs.conns[cid]; ok && wc.tp != nil {
		return wc.tp, true
	}
	return nil, false
}

func (cs *wsConns) remove(cid uint64) {
	cs.mu.Lock()
	delete(cs.conns, cid)
	cs.mu.Unlock()
}

func (cs *wsConns) closeAll() {
	cs.mu.RLock()
	conns := make([]*websocket.Conn, 0, len(cs.conns))
	for _, wc := range cs.conns {
		if wc.conn != nil {
			conns = append(conns, wc.conn)
		}
	}
	cs.mu.RUnlock()
	for _, c := range conns {
		c.CloseDetail(websocket.StatusGoAway, "server shutdown")
	}
}

// WebsocketVatel manages WebSocket endpoint registration and message routing.
type WebsocketVatel struct {
	isPublicAccessAllowed bool
	ws                    websocket.Server
	path                  map[string]*Endpoint
	cfg                   WsOption
	conns                 wsConns
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

// WithLogger sets the logger of WS endpoints; their lines get "layer":"ws".
// Without it WS endpoints log to the logger passed to BuildHandlers.
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
		path:  make(map[string]*Endpoint),
		conns: newWsConns(),
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
		return fmt.Errorf("WS endpoint %s: already registered", e.Path)
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
	compileCommon(v, e)
	e.wsLogger = l
	if wv.cfg.log != nil {
		wl := wv.cfg.log.With().Str("method", e.Method).Str("path", e.Path).Logger()
		e.wsLogger = &wl
	}

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

// OnTokenUpdate registers a callback invoked by UpdateAccessToken after the
// token is decoded and before its payload is stored for the connection cid.
// Returning an error rejects the token: the payload is not stored (the
// connection keeps its previous payload, if any) and the error is returned
// from UpdateAccessToken, and so sent to the client of the updateAccessToken
// endpoint.
func (wv *WebsocketVatel) OnTokenUpdate(f func(cid uint64, tp AuthPayload) error) {
	wv.callbackOnTokenUpdate = f
}

func (wv *WebsocketVatel) onOpen(c *websocket.Conn) {
	wv.conns.add(c)
	if wv.callbackOnOpen != nil {
		wv.callbackOnOpen(c)
	}
}

func (wv *WebsocketVatel) onClose(c *websocket.Conn, err error) {
	wv.conns.remove(c.ID())
	if wv.callbackOnClose != nil {
		wv.callbackOnClose(c, err)
	}
}

// Shutdown closes all active WebSocket connections with a "going away" status.
// Call this before shutting down the HTTP server to allow clients to reconnect.
func (wv *WebsocketVatel) Shutdown() {
	wv.conns.closeAll()
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
//
// Deprecated: vatel does not use it; the updateAccessToken endpoint reads
// {"mid": ..., "at": ...}. Kept for compatibility.
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

	verbose := e.v.cfg.verboseError

	lo := effectiveLogLevel(e.LogOptions)

	zco := e.wsLogger.With().Str("client", c.RemoteAddr().String())
	if e.v.cfg.logRequestID {
		zco = zco.Uint64("connectionId", c.ID())
	}
	zc := zco

	// Restore token payload from the connection table (set by UpdateAccessToken).
	if tp, ok := wv.conns.payload(c.ID()); ok {
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
			e.wsWriteErrorResponse(c, lo, verbose, &zc, ctx.Created(), err)
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
		if err := decodeWSInput(msg.Data, input.Input(), e.inputFields.HasRules()); err != nil {
			e.wsWriteErrorResponse(c, lo, verbose, &zc, ctx.Created(), err)
			return
		}
	}

	// ── Handler ──────────────────────────────────────────────────────────────
	if err := h.Handle(ctx); err != nil {
		e.wsWriteErrorResponse(c, lo, verbose, &zc, ctx.Created(), err)
		return
	}

	// ── Response serialization ───────────────────────────────────────────────
	if e.hasRespBody {
		res := h.(Resulter).Result()
		if !isNilResult(res) {
			if err := e.writeJSON(c, lo, res, &zc); err != nil {
				e.wsWriteErrorResponse(c, lo, verbose, &zc, ctx.Created(), err)
				return
			}
		}
	}

	// ── Exit logging + metrics ───────────────────────────────────────────────
	dur := time.Since(ctx.Created())
	if lo&LogExit == LogExit {
		logExit(zc, lo, ctx.VisitValues, nil, dur)
	}
	e.reportMetric(200, dur, 0)
}

// decodeWSInput unmarshals the "data" field of a WebSocket message into dest.
// A malformed or missing payload is a client error: ErrInvalidRequestBody
// (400). masked reports whether the type of dest has mask rules (see
// jsonDecodeError).
func decodeWSInput(data []byte, dest interface{}, masked bool) error {
	if err := json.Unmarshal(data, dest); err != nil {
		return jsonDecodeError(err, masked, "ws message data is not valid JSON")
	}
	return nil
}

// wsWriteErrorResponse logs an error (at level lo) and sends it to the
// WebSocket client w. The status and the client JSON are resolved as for
// HTTP (see errorResponse).
func (e *Endpoint) wsWriteErrorResponse(w io.Writer, lo LogOption, verbose bool, zc *zerolog.Context, created time.Time, err error) {
	if err == nil {
		return
	}

	statusCode, body, _ := e.errorResponse(err, verbose)

	if isErrorLogged(lo, statusCode) {
		zl := zc.RawJSON("err", errors.ToJSON(err, errors.WithAttributes(errors.ServerOutputFormat))).Logger()
		zl.Error().Msg("ws request failed")
	}

	w.Write(body) //nolint:errcheck

	e.reportMetric(statusCode, time.Since(created), len(body))
	e.alarm(statusCode, err)
}

// UpdateAccessToken decodes a new access token and calls the OnTokenUpdate
// callback, if any; unless the callback returns an error, it stores the
// payload for the connection cid. A token that cannot be decoded is rejected
// with ErrUnauthorized. Without WithTokenDecoder it does nothing.
func (wv *WebsocketVatel) UpdateAccessToken(cid uint64, token string) error {
	if wv.cfg.td == nil {
		return nil
	}
	at, err := wv.cfg.td.Decode([]byte(token))
	if err != nil {
		return ErrUnauthorized.Wrap(err)
	}
	tp := at.ApplicationPayload()
	if wv.callbackOnTokenUpdate != nil {
		if err := wv.callbackOnTokenUpdate(cid, tp); err != nil {
			return err
		}
	}
	wv.conns.setPayload(cid, tp)
	return nil
}
