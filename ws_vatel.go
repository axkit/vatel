package vatel

import (
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/axkit/errors"
	"github.com/regorov/websocket"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

// wsConns is the thread-safe table of live WebSocket connections: the
// connection and its network connection, to close it on shutdown, and its
// token payload, which persists authentication state across messages on the
// same connection.
type wsConns struct {
	mu     sync.RWMutex
	conns  map[uint64]*wsConn
	closed bool // closeAll was called: no new connection is accepted
}

type wsConn struct {
	conn      *websocket.Conn // nil when a payload is set for an unknown connection
	nc        net.Conn        // the network connection of conn
	tp        AuthPayload
	goingAway bool // closed by closeAll: its messages are not served
}

var (
	errWsShutdown    = stderrors.New("server shutdown")
	errWsDuplicateID = stderrors.New("duplicate connection id")
)

// wsCloseTimeout is how long a connection closed by the server waits for the
// client to answer the close frame (and for a stalled write) before its
// network reads and writes fail.
const wsCloseTimeout = time.Second

func newWsConns() wsConns {
	return wsConns{conns: make(map[uint64]*wsConn)}
}

// add registers the connection c with its network connection nc. A payload
// stored for its ID before it opened becomes its payload. After closeAll it
// returns errWsShutdown, and errWsDuplicateID when a live connection has the
// ID of c; c is not registered then.
func (cs *wsConns) add(c *websocket.Conn, nc net.Conn) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.closed {
		return errWsShutdown
	}
	wc, ok := cs.conns[c.ID()]
	switch {
	case !ok:
		cs.conns[c.ID()] = &wsConn{conn: c, nc: nc}
	case wc.conn != nil:
		return errWsDuplicateID
	default:
		wc.conn, wc.nc = c, nc
	}
	return nil
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

// serving reports whether the messages of c are served: c is registered and
// not being closed by closeAll. It returns the token payload of c, if any.
func (cs *wsConns) serving(c *websocket.Conn) (AuthPayload, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if wc, ok := cs.conns[c.ID()]; ok && wc.conn == c && !wc.goingAway {
		return wc.tp, true
	}
	return nil, false
}

// remove unregisters the connection c. It reports whether c was registered
// and whether closeAll closed it.
func (cs *wsConns) remove(c *websocket.Conn) (registered, goingAway bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	wc, ok := cs.conns[c.ID()]
	if !ok || wc.conn != c {
		return false, false
	}
	delete(cs.conns, c.ID())
	return true, wc.goingAway
}

// closeAll makes add reject new connections and closes the registered ones
// with a "going away" status. It does not wait: each close runs in its own
// goroutine, as queueing the close frame blocks while the outbound queue of
// the connection is full. Only the first call does anything.
func (cs *wsConns) closeAll() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.closed {
		return
	}
	cs.closed = true
	for _, wc := range cs.conns {
		if wc.conn == nil {
			continue
		}
		wc.goingAway = true
		go closeConn(wc.conn, wc.nc, websocket.StatusGoAway, errWsShutdown.Error())
	}
}

// closeConn starts the closing handshake of the connection c, whose network
// connection is nc: it queues a close frame with status and reason, after
// which the writer of c stops. The client's answering close frame ends the
// connection; nc reads and writes fail after wsCloseTimeout, so a client that
// does not answer, or does not read, does not hold the connection open.
//
// The websocket library's Conn.Close and Conn.CloseDetail are not safe to call
// outside the goroutine of the connection: they close a channel that the
// library closes too when the client close frame arrives. closeConn does not
// close it and may run in any goroutine. It blocks while the outbound queue of
// c is full; the write deadline frees a writer stalled on the network.
func closeConn(c *websocket.Conn, nc net.Conn, status websocket.StatusCode, reason string) {
	if nc != nil {
		nc.SetDeadline(time.Now().Add(wsCloseTimeout)) //nolint:errcheck
	}
	fr := websocket.AcquireFrame()
	fr.SetClose()
	fr.SetStatus(status)
	fr.SetFin()
	fr.Write([]byte(reason)) //nolint:errcheck
	c.WriteFrame(fr)
}

// WebsocketVatel manages WebSocket endpoint registration and message routing.
type WebsocketVatel struct {
	v                     *Vatel          // set by BuildHandlers
	log                   *zerolog.Logger // logs message envelope errors; set by BuildHandlers
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

// WithTokenDecoder sets the decoder of tokens sent to the built-in
// updateAccessToken endpoint. Without it the endpoint stores nothing.
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
			EndpointMeta:        EndpointMeta{Path: "updateAccessToken", Method: methodWS, LogOptions: LogConfidential},
			WebsocketController: func() WebsocketHandler { return &AccessTokenUpdateHandler{wv: wv} },
		},
	}
}

// bind links wv to the Vatel v that serves it. Errors of a message envelope
// (no endpoint is known yet) are logged to the WithLogger logger, or to l.
func (wv *WebsocketVatel) bind(v *Vatel, l *zerolog.Logger) {
	wv.v = v
	wl := l.With().Str("method", methodWS).Logger()
	if wv.cfg.log != nil {
		wl = wv.cfg.log.With().Str("method", methodWS).Logger()
	}
	wv.log = &wl
}

// RegisterEndpoint is called by Vatel.BuildHandlers (registerEndpoints) for
// every endpoint with method "WS".
func (wv *WebsocketVatel) RegisterEndpoint(v *Vatel, e *Endpoint, l *zerolog.Logger) error {
	if _, ok := wv.path[e.Path]; ok {
		return fmt.Errorf("endpoint %s %s: already registered", methodWS, e.Path)
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
		return fmt.Errorf("endpoint %s %s: WithWsAuthFunc() must be set for endpoints declaring Perms", methodWS, e.Path)
	}

	if e.WebsocketController == nil {
		return fmt.Errorf("endpoint %s %s: missing WebsocketController", methodWS, e.Path)
	}
	c := e.WebsocketController()
	if isNilResult(c) {
		return fmt.Errorf("endpoint %s %s: WebsocketController returned nil", methodWS, e.Path)
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

// wsNetConnKey is the user value under which Upgrade passes the network
// connection to onOpen (the websocket library copies string-keyed user values
// of the request to the connection).
const wsNetConnKey = "vatel.wsNetConn"

// onOpen registers the connection c. A connection opened after Shutdown, or
// with the ID of a live connection (UpgradeWithID), is closed at once, without
// the OnOpen and OnClose callbacks, and its messages are not served.
func (wv *WebsocketVatel) onOpen(c *websocket.Conn) {
	nc, _ := c.UserValue(wsNetConnKey).(net.Conn)
	switch err := wv.conns.add(c, nc); err {
	case nil:
	case errWsShutdown:
		closeConn(c, nc, websocket.StatusGoAway, err.Error())
		return
	default:
		if wv.log != nil {
			wv.log.Warn().Uint64("connectionId", c.ID()).Str("client", c.RemoteAddr().String()).
				Msg("ws connection rejected: duplicate connection id")
		}
		closeConn(c, nc, websocket.StatusViolation, err.Error())
		return
	}
	if wv.callbackOnOpen != nil {
		wv.callbackOnOpen(c)
	}
}

func (wv *WebsocketVatel) onClose(c *websocket.Conn, err error) {
	registered, goingAway := wv.conns.remove(c)
	if !registered {
		return
	}
	if goingAway {
		err = nil // closed by Shutdown: err is the client's answer or a timeout
	}
	if wv.callbackOnClose != nil {
		wv.callbackOnClose(c, err)
	}
}

// Shutdown closes all active WebSocket connections with a "going away" status,
// so clients can reconnect to another instance; their further messages are
// not served, and a connection opened later is closed at once. It does not
// wait for the connections to close: a client that does not answer the close
// frame (or does not read) is disconnected after about one second.
// Vatel.Shutdown calls it; calling it again does nothing.
func (wv *WebsocketVatel) Shutdown() {
	wv.conns.closeAll()
}

// Upgrade upgrades an HTTP connection to WebSocket.
func (wv *WebsocketVatel) Upgrade(ctx *fasthttp.RequestCtx) {
	ctx.SetUserValue(wsNetConnKey, ctx.Conn())
	wv.ws.Upgrade(ctx)
}

// UpgradeWithID upgrades an HTTP connection to WebSocket with a caller-supplied
// ID. The ID must not be the ID of a live connection: such a connection is
// closed at once with a "policy violation" status (1008).
func (wv *WebsocketVatel) UpgradeWithID(ctx *fasthttp.RequestCtx, id uint64) {
	ctx.SetUserValue(wsNetConnKey, ctx.Conn())
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

func (wv *WebsocketVatel) onMessage(c *websocket.Conn, _ bool, data []byte) {
	tp, ok := wv.conns.serving(c)
	if !ok {
		return // rejected by onOpen or closed by Shutdown
	}
	msg, e, err := wv.route(data)
	if err != nil {
		wv.writeEnvelopeError(c, msg.Path, err)
		return
	}
	wv.serveMessage(c, e, msg, tp)
}

// route decodes the message envelope data and returns it with the endpoint
// of its path.
func (wv *WebsocketVatel) route(data []byte) (WebsocketRequest, *Endpoint, error) {
	var msg WebsocketRequest
	if err := json.Unmarshal(data, &msg); err != nil {
		// The message may carry data of a masked type: the error must not
		// quote it.
		return msg, nil, jsonDecodeError(err, true, "ws message is not valid JSON")
	}
	if msg.Path == "" {
		return msg, nil, ErrInvalidRequestBody.New().Msg("ws message has no path")
	}
	e, ok := wv.path[msg.Path]
	if !ok {
		return msg, nil, ErrUnknownPath.New()
	}
	return msg, e, nil
}

// serveMessage serves the message msg of connection c with the endpoint e:
// authorization, input decoding, the handler, the reply, the exit log line
// and the metric. tp is the token payload of c, if any.
func (wv *WebsocketVatel) serveMessage(c *websocket.Conn, e *Endpoint, msg WebsocketRequest, tp AuthPayload) {
	ctx := NewWsContext(c)
	verbose := e.v.cfg.verboseError
	lo := effectiveLogLevel(e.LogOptions)

	zco := e.wsLogger.With().Str("client", c.RemoteAddr().String())
	if e.v.cfg.logRequestID {
		zco = zco.Uint64("connectionId", c.ID())
	}
	zc := zco

	// Restore token payload from the connection table (set by UpdateAccessToken).
	if tp != nil {
		ctx.SetTokenPayload(tp)
	}

	zc, err := wv.authorize(ctx, e, zc)
	if err != nil {
		e.wsWriteErrorResponse(c, lo, verbose, zc, ctx.Created(), err)
		return
	}

	if lo&LogEnter == LogEnter {
		zl := zc.Logger()
		zl.Debug().Msg("new ws request")
		zc = zco
	}

	h := e.WebsocketController()
	if input, ok := h.(Inputer); ok {
		in := input.Input()
		if err := decodeWSInput(msg.Data, in, e.inputFields.HasRules()); err != nil {
			e.wsWriteErrorResponse(c, lo, verbose, zc, ctx.Created(), err)
			return
		}
		if err := validate(in); err != nil {
			e.wsWriteErrorResponse(c, lo, verbose, zc, ctx.Created(), err)
			return
		}
	}

	if err := h.Handle(ctx); err != nil {
		e.wsWriteErrorResponse(c, lo, verbose, zc, ctx.Created(), err)
		return
	}

	zc, size, err := e.writeWSResult(c, h, lo, zc)
	if err != nil {
		e.wsWriteErrorResponse(c, lo, verbose, zc, ctx.Created(), err)
		return
	}

	dur := time.Since(ctx.Created())
	if lo&LogExit == LogExit {
		logExit(zc, lo, ctx.VisitValues, nil, dur)
	}
	e.reportMetric(fasthttp.StatusOK, dur, size)
}

// authorize calls the WithWsAuthFunc function, if any, for the endpoint e and
// returns zc with the permissions it requires.
func (wv *WebsocketVatel) authorize(ctx WebsocketContext, e *Endpoint, zc zerolog.Context) (zerolog.Context, error) {
	if wv.cfg.authFunc == nil {
		return zc, nil
	}
	switch len(e.Perms) {
	case 0:
	case 1:
		zc = zc.Str("perm", e.Perms[0])
	default:
		zc = zc.Strs("perms", e.Perms)
	}
	return zc, wv.cfg.authFunc(ctx, e.Path, e.Perms)
}

// writeWSResult sends the result of h, if it has one, to the client w. It
// returns zc with the logged result and the size of the reply.
func (e *Endpoint) writeWSResult(w io.Writer, h WebsocketHandler, lo LogOption, zc zerolog.Context) (zerolog.Context, int, error) {
	if !e.hasRespBody {
		return zc, 0, nil
	}
	res := h.(Resulter).Result()
	if isNilResult(res) {
		return zc, 0, nil
	}
	return e.writeJSON(w, lo, res, zc)
}

// writeEnvelopeError logs an error of the message envelope (invalid JSON, no
// path, an unknown path) and sends it to the client c. No endpoint is known,
// so no metric is reported; path is the requested path, if any.
func (wv *WebsocketVatel) writeEnvelopeError(c *websocket.Conn, path string, err error) {
	if wv.v == nil {
		// Not served by a Vatel (BuildHandlers was not called).
		c.Write(ToClientJSON(err)) //nolint:errcheck
		return
	}
	_, body, _ := wv.v.errorResponse(err, wv.v.cfg.verboseError)
	zc := wv.log.With().Str("path", path).Str("client", c.RemoteAddr().String())
	if wv.v.cfg.logRequestID {
		zc = zc.Uint64("connectionId", c.ID())
	}
	zl := zc.RawJSON("err", errors.ToJSON(err, errors.WithAttributes(errors.ServerOutputFormat))).Logger()
	zl.Error().Msg("ws request failed")
	c.Write(body) //nolint:errcheck
}

// decodeWSInput unmarshals the "data" field of a WebSocket message into dest.
// A malformed or missing payload is a client error: ErrInvalidRequestBody
// (400). masked reports whether the type of dest has mask rules (see
// jsonDecodeError).
func decodeWSInput(data []byte, dest any, masked bool) error {
	if err := json.Unmarshal(data, dest); err != nil {
		return jsonDecodeError(err, masked, "ws message data is not valid JSON")
	}
	return nil
}

// wsWriteErrorResponse logs an error (at level lo) and sends it to the
// WebSocket client w. The status and the client JSON are resolved as for
// HTTP (see errorResponse).
func (e *Endpoint) wsWriteErrorResponse(w io.Writer, lo LogOption, verbose bool, zc zerolog.Context, created time.Time, err error) {
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
