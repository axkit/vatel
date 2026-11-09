package vatel

import (
	"cmp"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	mu          sync.RWMutex
	conns       map[uint64]*wsConn
	closed      bool          // closeAll was called: no new connection is accepted
	drained     chan struct{} // closed once closeAll was called and no connection is registered
	authTimeout time.Duration // see WithWsAuthTimeout; 0: none
}

type wsConn struct {
	conn      *websocket.Conn // nil when a payload is set for an unknown connection
	nc        net.Conn        // the network connection of conn
	tp        AuthPayload
	exp       time.Time   // expiry of tp (Expirer), zero if none
	ver       uint64      // number of payloads stored: UpdateAccessToken compares it
	closing   bool        // closed by the server: its messages are not served
	goingAway bool        // closed by closeAll: OnClose gets a nil error
	timer     *time.Timer // closes the live connection when tp expires, or, without a payload, after the auth timeout
}

var (
	errWsShutdown    = stderrors.New("server shutdown")
	errWsDuplicateID = stderrors.New("duplicate connection id")
)

// wsCloseTimeout is how long a write stalled on a client that does not read
// may hold up a connection closed by the server: its network reads and writes
// fail after it.
const wsCloseTimeout = time.Second

// DefaultWsWriteTimeout is the default write timeout of a WebSocket frame (see
// WithWsWriteTimeout).
const DefaultWsWriteTimeout = 10 * time.Second

func newWsConns() wsConns {
	return wsConns{conns: make(map[uint64]*wsConn), drained: make(chan struct{})}
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
		wc = &wsConn{conn: c, nc: nc}
		cs.conns[c.ID()] = wc
	case wc.conn != nil:
		return errWsDuplicateID
	default:
		wc.conn, wc.nc = c, nc
	}
	cs.armTimer(c.ID(), wc)
	return nil
}

// lookup returns the entry of the connection cid (nil if there is none), its
// payload and the version of the payload, for storePayload.
func (cs *wsConns) lookup(cid uint64) (wc *wsConn, tp AuthPayload, ver uint64) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if wc = cs.conns[cid]; wc != nil {
		return wc, wc.tp, wc.ver
	}
	return nil, nil, 0
}

// storePayload stores the token payload tp, which expires at exp (zero:
// never), for the connection cid, unless its entry is no longer seen or its
// payload is no longer of version ver (see lookup): another payload was stored
// meanwhile, or the connection closed. It reports whether tp was stored.
func (cs *wsConns) storePayload(cid uint64, seen *wsConn, ver uint64, tp AuthPayload, exp time.Time) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	wc := cs.conns[cid]
	if wc != seen || (wc != nil && wc.ver != ver) {
		return false
	}
	if wc == nil {
		wc = &wsConn{}
		cs.conns[cid] = wc
	}
	wc.tp, wc.exp = tp, exp
	wc.ver++
	cs.armTimer(cid, wc)
	return true
}

// armTimer (re)starts the timer of wc, the entry of the connection cid: if
// its payload expires, the connection is closed with WsStatusAuthRequired
// then; without a payload, after cs.authTimeout. The timer does nothing if
// another payload is stored before it fires. Only a live connection that is
// not closing gets a timer. cs.mu must be held.
func (cs *wsConns) armTimer(cid uint64, wc *wsConn) {
	wc.stopTimer()
	if wc.conn == nil || wc.closing {
		return
	}
	var d time.Duration
	var reason string
	switch {
	case wc.ver == 0 && cs.authTimeout > 0:
		d, reason = cs.authTimeout, "authentication timeout"
	case wc.ver > 0 && !wc.exp.IsZero():
		d, reason = time.Until(wc.exp), "token expired"
	default:
		return
	}
	ver := wc.ver
	wc.timer = time.AfterFunc(d, func() {
		cs.mu.Lock()
		if cs.conns[cid] != wc || wc.ver != ver || !wc.startClose() {
			cs.mu.Unlock()
			return
		}
		c, nc := wc.conn, wc.nc
		cs.mu.Unlock()
		closeConn(c, nc, WsStatusAuthRequired, reason)
	})
}

// stopTimer stops the timer of wc, if any. The mutex of its table must be
// held.
func (wc *wsConn) stopTimer() {
	if wc.timer != nil {
		wc.timer.Stop()
		wc.timer = nil
	}
}

// startClose marks the live connection of wc as closing: its messages are no
// longer served and its timer is stopped. It reports false when wc has no
// connection or it is already closing. The mutex of its table must be held.
func (wc *wsConn) startClose() bool {
	if wc.conn == nil || wc.closing {
		return false
	}
	wc.closing = true
	wc.stopTimer()
	return true
}

// startClose marks the live connection cid as closing (see wsConn.startClose)
// and returns it with its network connection. With c set, the connection
// must be c. It reports false if there is no such connection, or it is
// already closing.
func (cs *wsConns) startClose(cid uint64, c *websocket.Conn) (*websocket.Conn, net.Conn, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	wc, ok := cs.conns[cid]
	if !ok || (c != nil && wc.conn != c) || !wc.startClose() {
		return nil, nil, false
	}
	return wc.conn, wc.nc, true
}

// serving reports whether the messages of c are served: c is registered and
// not being closed by the server. It returns the token payload of c, if any.
func (cs *wsConns) serving(c *websocket.Conn) (AuthPayload, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if wc, ok := cs.conns[c.ID()]; ok && wc.conn == c && !wc.closing {
		return wc.tp, true
	}
	return nil, false
}

// wsConnInfo is a live connection as VisitConns passes it.
type wsConnInfo struct {
	cid uint64
	tp  AuthPayload
}

// live returns the live connections that are not closing, by ID.
func (cs *wsConns) live() []wsConnInfo {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	res := make([]wsConnInfo, 0, len(cs.conns))
	for cid, wc := range cs.conns {
		if wc.conn != nil && !wc.closing {
			res = append(res, wsConnInfo{cid, wc.tp})
		}
	}
	slices.SortFunc(res, func(a, b wsConnInfo) int { return cmp.Compare(a.cid, b.cid) })
	return res
}

// remove unregisters the connection c and stops its timer. It reports
// whether c was registered and whether closeAll closed it.
func (cs *wsConns) remove(c *websocket.Conn) (registered, goingAway bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	wc, ok := cs.conns[c.ID()]
	if !ok || wc.conn != c {
		return false, false
	}
	wc.stopTimer()
	delete(cs.conns, c.ID())
	cs.checkDrained()
	return true, wc.goingAway
}

// checkDrained closes cs.drained once closeAll was called and no connection
// is registered. cs.mu must be held.
func (cs *wsConns) checkDrained() {
	if !cs.closed {
		return
	}
	select {
	case <-cs.drained:
		return
	default:
	}
	for _, wc := range cs.conns {
		if wc.conn != nil {
			return
		}
	}
	close(cs.drained)
}

// closeAll makes add reject new connections and closes the registered ones
// with a "going away" status, stopping their timers; a connection already
// being closed by the server keeps its status. It does not wait: each close
// runs in its own goroutine, as queueing the close frame blocks while the
// outbound queue of the connection is full; cs.drained is closed when all of
// them have closed. Only the first call does anything.
func (cs *wsConns) closeAll() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.closed {
		return
	}
	cs.closed = true
	for _, wc := range cs.conns {
		if !wc.startClose() {
			continue
		}
		wc.goingAway = true
		go closeConn(wc.conn, wc.nc, websocket.StatusGoAway, errWsShutdown.Error())
	}
	cs.checkDrained()
}

// wsMaxCloseReason is the longest close reason in bytes: the payload of a
// close frame (a control frame) is at most 125 bytes, two of them the status.
const wsMaxCloseReason = 123

// truncateCloseReason returns reason as valid UTF-8 cut to wsMaxCloseReason
// bytes on a character boundary. A longer or invalid reason makes browsers
// fail the connection (1002/1006) and lose the status.
func truncateCloseReason(reason string) string {
	reason = strings.ToValidUTF8(reason, "")
	if len(reason) <= wsMaxCloseReason {
		return reason
	}
	i := wsMaxCloseReason
	for i > 0 && !utf8.RuneStart(reason[i]) {
		i--
	}
	return reason[:i]
}

// closeConn closes the connection c, whose network connection is nc, with
// status and reason (cut by truncateCloseReason): Conn.CloseDetail (safe from
// any goroutine) queues the close frame, after which writes to c fail with
// websocket.ErrConnClosed, and the library closes nc once the queued frames
// are written (at most about one second later). CloseDetail blocks while the
// outbound queue of c is full, and a client that does not read can stall the
// writer of c, so nc reads and writes first get a deadline of wsCloseTimeout.
func closeConn(c *websocket.Conn, nc net.Conn, status websocket.StatusCode, reason string) {
	if nc != nil {
		nc.SetDeadline(time.Now().Add(wsCloseTimeout)) //nolint:errcheck
	}
	c.CloseDetail(status, truncateCloseReason(reason))
}

// Close statuses of the WebSocket connections closed by the server for
// authentication reasons; WsCloseError carries one. Applications may define
// their own in the 4000–4999 range. A client reacts to them as follows.
const (
	// WsStatusAuthRequired (4001): the connection has no valid token (its
	// token expired, or none came within WithWsAuthTimeout). The client gets
	// a fresh token and reconnects.
	WsStatusAuthRequired websocket.StatusCode = 4001
	// WsStatusForbidden (4003): the connection may not continue (another
	// user, access revoked). The client does not reconnect.
	WsStatusForbidden websocket.StatusCode = 4003
	// WsStatusContextChanged (4009): the token is of another security
	// context (e.g. organization) than the connection. The client gets a
	// token of the context it needs and opens a new connection with it.
	WsStatusContextChanged websocket.StatusCode = 4009
)

// WsCloseError is an error that closes the WebSocket connection. When the
// handler of a message (including the built-in updateAccessToken, through
// OnTokenUpdate, and the WithWsAuthFunc function) returns an error with a
// *WsCloseError in its chain, the client first gets the error reply to the
// message, then the connection is closed with Code and Reason.
//
// The reply is built from Err when the chain has an *errors.Error with a
// status; otherwise from Code: WsStatusAuthRequired gives ErrUnauthorized
// (401), WsStatusForbidden ErrForbidden (403), WsStatusContextChanged
// ErrWsContextChanged (409), any other code 400. It is never alarmed.
//
// Reason is sent to the client in the close frame, cut to 123 bytes: it must
// not carry sensitive details; put them in Err.
type WsCloseError struct {
	Code   websocket.StatusCode
	Reason string
	Err    error // optional
}

// NewWsCloseError returns a WsCloseError closing the connection with code and
// reason; err (may be nil) is the cause.
func NewWsCloseError(code websocket.StatusCode, reason string, err error) *WsCloseError {
	return &WsCloseError{Code: code, Reason: reason, Err: err}
}

func (e *WsCloseError) Error() string {
	s := "websocket close " + strconv.Itoa(int(e.Code))
	if e.Reason != "" {
		s += " " + strconv.Quote(e.Reason)
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *WsCloseError) Unwrap() error { return e.Err }

// errWsClosed is the reply to a message whose WsCloseError has a code
// without its own error.
var errWsClosed = errors.Template("bad request").StatusCode(400).Severity(errors.Tiny)

// wsReplyError returns the error sent to the client for err and the
// *WsCloseError in its chain, if any (see WsCloseError).
func wsReplyError(err error) (error, *WsCloseError) {
	var wce *WsCloseError
	if !stderrors.As(err, &wce) {
		return err, nil
	}
	if xe := vatelError(err); xe != nil && errors.Serialize(xe).StatusCode != 0 {
		return err, wce
	}
	t := errWsClosed
	switch wce.Code {
	case WsStatusAuthRequired:
		t = ErrUnauthorized
	case WsStatusForbidden:
		t = ErrForbidden
	case WsStatusContextChanged:
		t = ErrWsContextChanged
	}
	return t.Wrap(err), wce
}

// Expirer is implemented by a token payload (AuthPayload) that expires. The
// WebSocket connection whose payload has a non-zero ExpiresAt is closed with
// WsStatusAuthRequired at that time, unless the client sends a new token
// (updateAccessToken) before; an already expired payload is rejected by
// UpdateAccessToken with ErrUnauthorized.
type Expirer interface {
	ExpiresAt() time.Time
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
	callbackOnTokenUpdate func(cid uint64, prev, next AuthPayload) error
}

// WsOption holds WebSocket-specific configuration.
type WsOption struct {
	log             *zerolog.Logger
	td              TokenDecoder
	authFunc        func(ctx WebsocketContext, path string, requiredPerms []string) error
	writeTimeout    time.Duration
	writeTimeoutSet bool
	authTimeout     time.Duration
}

// WithWsWriteTimeout sets how long writing one frame to a WebSocket client may
// take; the default is DefaultWsWriteTimeout, 0 means no limit. A write of a
// client that does not read blocks once the outbound queue of its connection
// (128 frames) is full; when the timeout expires the write fails and the
// connection ends, so the handler writing to it does not block forever.
func WithWsWriteTimeout(d time.Duration) func(*WsOption) {
	return func(o *WsOption) { o.writeTimeout, o.writeTimeoutSet = d, true }
}

// WithWsAuthTimeout closes a connection that has no token payload d after it
// opened (no successful UpdateAccessToken, nor a payload stored for its ID
// before it opened) with WsStatusAuthRequired. A rejected token does not
// count. The default 0 means no limit. It has no effect without
// WithTokenDecoder.
func WithWsAuthTimeout(d time.Duration) func(*WsOption) {
	return func(o *WsOption) { o.authTimeout = d }
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
	if !wv.cfg.writeTimeoutSet {
		wv.cfg.writeTimeout = DefaultWsWriteTimeout
	}
	if wv.cfg.td != nil {
		wv.conns.authTimeout = max(wv.cfg.authTimeout, 0)
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
// token is decoded and before its payload next is stored for the connection
// cid; prev is the payload stored for it so far, nil for the first token.
// Returning an error rejects the token: the payload is not stored (the
// connection keeps its previous payload, if any) and the error is returned
// from UpdateAccessToken, and so sent to the client of the updateAccessToken
// endpoint. A *WsCloseError then also closes the connection after the reply,
// e.g. to keep one security context (user, organization) per connection:
//
//	wv.OnTokenUpdate(func(_ uint64, prev, next vatel.AuthPayload) error {
//		if prev == nil {
//			return nil // first token
//		}
//		p, n := prev.(*Claims), next.(*Claims)
//		switch {
//		case p.UserID != n.UserID:
//			return vatel.NewWsCloseError(vatel.WsStatusForbidden, "user changed", nil)
//		case p.OrgID != n.OrgID:
//			return vatel.NewWsCloseError(vatel.WsStatusContextChanged, "organization changed", nil)
//		}
//		return nil
//	})
//
// The callback runs without a lock held; if another payload is stored for cid
// meanwhile (a concurrent UpdateAccessToken), or the connection closes,
// UpdateAccessToken returns ErrWsTokenConflict and stores nothing.
func (wv *WebsocketVatel) OnTokenUpdate(f func(cid uint64, prev, next AuthPayload) error) {
	wv.callbackOnTokenUpdate = f
}

// wsNetConnKey is the user value under which Upgrade passes the network
// connection to onOpen (the websocket library copies string-keyed user values
// of the request to the connection).
const wsNetConnKey = "vatel.wsNetConn"

// onOpen registers the connection c. A connection opened after Shutdown, or
// with the ID of a live connection (UpgradeWithID), is closed at once, without
// the OnOpen and OnClose callbacks, and its messages are not served. A panic
// in the OnOpen callback is recovered (see runCallback) and closes c with
// status 1011; its messages are not served, OnClose is still called for it.
func (wv *WebsocketVatel) onOpen(c *websocket.Conn) {
	// No frame is written before onOpen returns, so setting the timeout here
	// does not race with the writer of c.
	c.WriteTimeout = wv.cfg.writeTimeout
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
		if wv.runCallback("OnOpen", c, func() { wv.callbackOnOpen(c) }) {
			wv.closeLive(c, websocket.StatusUnexpected, "internal error")
		}
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
		wv.runCallback("OnClose", c, func() { wv.callbackOnClose(c, err) })
	}
}

// runCallback calls f, the OnOpen or OnClose callback named name for the
// connection c, and recovers its panic: the panic value and the stack are
// logged, the Alarmer is called, and runCallback reports true. A panic during
// the recovery itself is dropped.
func (wv *WebsocketVatel) runCallback(name string, c *websocket.Conn, f func()) (panicked bool) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		panicked = true
		defer func() { recover() }() //nolint:errcheck // a panic while reporting is dropped
		if wv.log != nil {
			zc := wv.log.With().Str("callback", name).Uint64("connectionId", c.ID())
			zl := withPanic(zc, r).Logger()
			zl.Error().Msg("ws callback panicked")
		}
		if wv.v != nil && wv.v.cfg.ala != nil {
			wv.v.cfg.ala.Alarm(panicError(r))
		}
	}()
	f()
	return false
}

// closeLive closes the registered connection c with status and reason, unless
// it is already closing: its messages are no longer served.
func (wv *WebsocketVatel) closeLive(c *websocket.Conn, status websocket.StatusCode, reason string) {
	if c, nc, ok := wv.conns.startClose(c.ID(), c); ok {
		closeConn(c, nc, status, reason)
	}
}

// CloseConn closes the live connection cid with status and reason, e.g. when
// its user was removed from the organization, logged out, or its token was
// revoked; its further messages are not served (the message being handled is
// finished), and OnClose is called once it has closed. Reason is sent to the
// client: it is cut to 123 bytes and must not carry sensitive details. It
// reports false if there is no such connection or it is already closing. It
// does not wait for the close and is safe from any goroutine, including a
// VisitConns callback.
func (wv *WebsocketVatel) CloseConn(cid uint64, status websocket.StatusCode, reason string) bool {
	c, nc, ok := wv.conns.startClose(cid, nil)
	if ok {
		// Queueing the close frame blocks while the outbound queue is full.
		go closeConn(c, nc, status, reason)
	}
	return ok
}

// VisitConns calls f for each live connection (not being closed), in the
// order of IDs, with its ID and its token payload (nil without one), until f
// returns false. f is called on a snapshot without a lock held, so it may call
// CloseConn; a connection may close meanwhile. Use it to find the connections
// of a user or an organization from their payloads.
func (wv *WebsocketVatel) VisitConns(f func(cid uint64, tp AuthPayload) bool) {
	for _, ci := range wv.conns.live() {
		if !f(ci.cid, ci.tp) {
			return
		}
	}
}

// Shutdown closes all active WebSocket connections with a "going away" status,
// so clients can reconnect to another instance; their further messages are
// not served, the message being handled is finished (its writes after the
// close fail), and a connection opened later is closed at once. It does not
// wait for the connections to close: a client that does not answer the close
// frame (or does not read) is disconnected after about one second.
// Vatel.Shutdown calls it and waits, within its context, until every
// connection has closed; calling it again does nothing.
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
//
// A panic is recovered: the client gets ErrInternalError (500, VTL-0005), the
// panic value and the stack are logged, the metric is reported, the Alarmer is
// called, and the connection goes on serving messages.
func (wv *WebsocketVatel) serveMessage(c *websocket.Conn, e *Endpoint, msg WebsocketRequest, tp AuthPayload) {
	ctx := NewWsContext(c)
	verbose := e.v.cfg.verboseError
	lo := effectiveLogLevel(e.LogOptions)

	zco := e.wsLogger.With().Str("client", c.RemoteAddr().String())
	if e.v.cfg.logRequestID {
		zco = zco.Uint64("connectionId", c.ID())
	}
	zc := zco
	defer func() {
		if r := recover(); r != nil {
			e.wsRecoverPanic(c, lo, verbose, zco, ctx.Created(), r)
		}
	}()

	// Restore token payload from the connection table (set by UpdateAccessToken).
	if tp != nil {
		ctx.SetTokenPayload(tp)
	}

	zc, err := wv.authorize(ctx, e, zc)
	if err != nil {
		wv.writeError(c, e, lo, verbose, zc, ctx.Created(), err)
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
			wv.writeError(c, e, lo, verbose, zc, ctx.Created(), err)
			return
		}
		if err := validate(in); err != nil {
			wv.writeError(c, e, lo, verbose, zc, ctx.Created(), err)
			return
		}
	}

	if err := h.Handle(ctx); err != nil {
		wv.writeError(c, e, lo, verbose, zc, ctx.Created(), err)
		return
	}

	zc, size, err := e.writeWSResult(c, h, lo, zc)
	if err != nil {
		wv.writeError(c, e, lo, verbose, zc, ctx.Created(), err)
		return
	}

	dur := time.Since(ctx.Created())
	if lo&LogExit == LogExit {
		logExit(zc, lo, ctx.VisitValues, nil, dur)
	}
	e.reportMetric(fasthttp.StatusOK, dur, size)
}

// writeError sends the error err of a message of connection c served by the
// endpoint e (see wsWriteErrorResponse); a *WsCloseError in its chain then
// closes c: the close frame is queued after the reply, so the client gets
// both.
func (wv *WebsocketVatel) writeError(c *websocket.Conn, e *Endpoint, lo LogOption, verbose bool, zc zerolog.Context, created time.Time, err error) {
	e.wsWriteErrorResponse(c, lo, verbose, zc, created, err)
	var wce *WsCloseError
	if stderrors.As(err, &wce) {
		wv.closeLive(c, wce.Code, wce.Reason)
	}
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
// HTTP (see errorResponse); for an error with a *WsCloseError, see
// wsReplyError, and it is not alarmed.
func (e *Endpoint) wsWriteErrorResponse(w io.Writer, lo LogOption, verbose bool, zc zerolog.Context, created time.Time, err error) {
	if err == nil {
		return
	}
	err, wce := wsReplyError(err)
	e.wsWriteError(w, lo, verbose, zc, created, err, wce == nil)
}

// wsWriteError logs err (at level lo), sends it to the WebSocket client w,
// reports the metric and, with alarm, passes a 5xx err to the Alarmer.
func (e *Endpoint) wsWriteError(w io.Writer, lo LogOption, verbose bool, zc zerolog.Context, created time.Time, err error, alarm bool) {
	statusCode, body, _ := e.errorResponse(err, verbose)

	if isErrorLogged(lo, statusCode) {
		zl := zc.RawJSON("err", errors.ToJSON(err, errors.WithAttributes(errors.ServerOutputFormat))).Logger()
		zl.Error().Msg("ws request failed")
	}

	w.Write(body) //nolint:errcheck

	e.reportMetric(statusCode, time.Since(created), len(body))
	if alarm {
		e.alarm(statusCode, err)
	}
}

// wsRecoverPanic answers a WS message whose handling panicked with r: the
// client w gets ErrInternalError (see recoverPanic), the panic value and the
// stack are logged, the metric is reported and the Alarmer is called. A panic
// during the recovery itself is logged and dropped.
func (e *Endpoint) wsRecoverPanic(w io.Writer, lo LogOption, verbose bool, zc zerolog.Context, created time.Time, r any) {
	defer func() {
		if r := recover(); r != nil {
			zl := withPanic(zc, r).Logger()
			zl.Error().Msg("panic while recovering from a panic")
		}
	}()
	// A panic is a 500, even with a *WsCloseError value.
	e.wsWriteError(w, lo, verbose, withPanic(zc, r), created, panicError(r), true)
}

// UpdateAccessToken decodes a new access token for the connection cid and
// calls the OnTokenUpdate callback, if any, with the payload stored so far;
// unless the callback returns an error, it stores the new payload for cid. A
// token that cannot be decoded, or whose payload (Expirer) has expired, is
// rejected with ErrUnauthorized; if another payload was stored for cid while
// the callback ran, or the connection closed, it returns ErrWsTokenConflict.
// A rejected token is not stored: the connection keeps its payload. Storing a
// payload arms the expiry of the connection (Expirer) and ends its
// WithWsAuthTimeout. A payload stored for a cid without a live connection
// becomes the payload of the connection opened with that ID
// (UpgradeWithID). Without WithTokenDecoder it does nothing.
//
// A *WsCloseError returned by the callback is returned as is: the
// updateAccessToken endpoint then closes the connection after its reply; a
// direct caller closes it with CloseConn.
func (wv *WebsocketVatel) UpdateAccessToken(cid uint64, token string) error {
	if wv.cfg.td == nil {
		return nil
	}
	at, err := wv.cfg.td.Decode([]byte(token))
	if err != nil {
		return ErrUnauthorized.Wrap(err)
	}
	if at == nil {
		// A broken TokenDecoder: neither a token nor an error.
		return ErrUnauthorized.New().Msg("token decoder returned no token")
	}
	next := at.ApplicationPayload()
	var exp time.Time
	if ex, ok := next.(Expirer); ok {
		exp = ex.ExpiresAt()
		if !exp.IsZero() && !time.Now().Before(exp) {
			return ErrUnauthorized.New().Msg("token expired")
		}
	}
	wc, prev, ver := wv.conns.lookup(cid)
	if wv.callbackOnTokenUpdate != nil {
		if err := wv.callbackOnTokenUpdate(cid, prev, next); err != nil {
			return err
		}
	}
	if !wv.conns.storePayload(cid, wc, ver, next, exp) {
		return ErrWsTokenConflict.New()
	}
	return nil
}
