package vatel

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/fasthttp/router"
	"github.com/regorov/websocket"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

// syncBuffer is a bytes.Buffer safe for concurrent use: WS messages are
// logged from the websocket server goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// syncMetrics records metrics and alarms from concurrent goroutines.
type syncMetrics struct {
	mu      sync.Mutex
	status  map[string][]int
	size    map[string][]int
	alarmed []error
}

func (m *syncMetrics) ReportMetric(_, path string, statusCode int, _ float64, size int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.status == nil {
		m.status = make(map[string][]int)
		m.size = make(map[string][]int)
	}
	m.status[path] = append(m.status[path], statusCode)
	m.size[path] = append(m.size[path], size)
}

// sizes returns the response sizes reported for path.
func (m *syncMetrics) sizes(path string) []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.size[path]...)
}

func (m *syncMetrics) Alarm(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alarmed = append(m.alarmed, err)
}

func (m *syncMetrics) get(path string) ([]int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.status[path]...), len(m.alarmed)
}

type wsSecretHandler struct {
	Body[struct {
		Name string `json:"name"`
	}]
	Output[secretItem]
}

func (h *wsSecretHandler) Handle(WebsocketContext) error {
	h.Out = secretItem{Name: h.In.Name, Password: "p1"}
	return nil
}

type wsPINHandler struct {
	Body[struct {
		PIN int64 `json:"pin" mask:"-"`
	}]
}

func (h *wsPINHandler) Handle(WebsocketContext) error { return nil }

type wsValidatedInput struct {
	Name string `json:"name"`
}

func (in *wsValidatedInput) Validate() error {
	if in.Name == "" {
		return ErrValidationFailed.New().Set("reason", "name is required")
	}
	return nil
}

// wsValidatedHandler counts its calls in served.
type wsValidatedHandler struct {
	Body[wsValidatedInput]
	Output[wsValidatedInput]
	served *atomic.Int32
}

func (h *wsValidatedHandler) Handle(WebsocketContext) error {
	h.served.Add(1)
	h.Out = h.In
	return nil
}

type wsFailHandler struct{}

func (wsFailHandler) Handle(WebsocketContext) error { return stderrors.New("SECRET dsn") }

// wsSlowHandler holds the connection busy for a while.
type wsSlowHandler struct{}

func (wsSlowHandler) Handle(WebsocketContext) error {
	time.Sleep(150 * time.Millisecond)
	return nil
}

// wsPanicHandler panics.
type wsPanicHandler struct{}

func (wsPanicHandler) Handle(WebsocketContext) error { panic("SECRET ws panic") }

// wsFloodHandler writes more messages than the outbound queue holds.
type wsFloodHandler struct{}

func (wsFloodHandler) Handle(ctx WebsocketContext) error {
	for range 1000 {
		ctx.BodyWriter().Write([]byte(`{"flood":true}`)) //nolint:errcheck
	}
	return nil
}

// wsCloserHandler returns a WsCloseError with the code and reason of its
// input; with coded, its Err is an *errors.Error (400, VTL-0006).
type wsCloserHandler struct {
	Body[struct {
		Code   int    `json:"code"`
		Reason string `json:"reason"`
		Coded  bool   `json:"coded"`
	}]
}

func (h *wsCloserHandler) Handle(WebsocketContext) error {
	var err error
	if h.In.Coded {
		err = ErrValidationFailed.New().Msg("bad state")
	}
	return NewWsCloseError(websocket.StatusCode(h.In.Code), h.In.Reason, err)
}

type wsHarness struct {
	t       *testing.T
	client  *websocket.Client
	log     *syncBuffer
	metrics *syncMetrics
	wv      *WebsocketVatel
	v       *Vatel
	ln      *fasthttputil.InmemoryListener
	opened  atomic.Int32 // OnOpen calls
	served  atomic.Int32 // wsValidatedHandler calls
	closed  atomic.Int32 // OnClose calls

	openHook  atomic.Pointer[func()]                  // called by OnOpen, if set
	closeHook atomic.Pointer[func()]                  // called by OnClose, if set
	authHook  atomic.Pointer[func(path string) error] // its error is returned by the auth func, if set

	mu        sync.Mutex
	conns     []net.Conn // client connections, closed before the cleanup Shutdown
	authPaths []string
	authPerms [][]string
	authTP    []any
}

func newWSHarness(t *testing.T, opts ...func(*WsOption)) *wsHarness {
	t.Helper()
	h := &wsHarness{t: t, log: &syncBuffer{}, metrics: &syncMetrics{}}

	wv := NewWebsocketVatel(append([]func(*WsOption){
		WithTokenDecoder(&mockDecoder{token: &mockTokener{payload: &mockPayload{perms: []byte{1}}}}),
		WithWsAuthFunc(func(ctx WebsocketContext, path string, perms []string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.authPaths = append(h.authPaths, path)
			h.authPerms = append(h.authPerms, perms)
			h.authTP = append(h.authTP, ctx.TokenPayload())
			if f := h.authHook.Load(); f != nil {
				return (*f)(path)
			}
			return nil
		}),
	}, opts...)...)
	wv.OnOpen(func(*websocket.Conn) {
		h.opened.Add(1)
		if f := h.openHook.Load(); f != nil {
			(*f)()
		}
	})
	wv.OnClose(func(*websocket.Conn, error) {
		h.closed.Add(1)
		if f := h.closeHook.Load(); f != nil {
			(*f)()
		}
	})
	h.wv = wv
	v := NewVatel(WithWebsocketSupport(wv), WithMetricReporter(h.metrics), WithAlarmer(h.metrics))
	h.v = v
	v.Add(Endpoints{
		WS("secret", func() WebsocketHandler { return &wsSecretHandler{} }, WithLogOption(LogExit|LogRespBody)),
		WS("silent", func() WebsocketHandler { return &wsSecretHandler{} }, WithLogOption(LogSilent)),
		WS("perm", func() WebsocketHandler { return &wsSecretHandler{} }, Perms("orders:read"), WithLogOption(LogExit)),
		WS("fail", func() WebsocketHandler { return wsFailHandler{} }, WithLogOption(LogExit)),
		WS("pin", func() WebsocketHandler { return &wsPINHandler{} }, WithLogOption(LogExit)),
		WS("slow", func() WebsocketHandler { return wsSlowHandler{} }),
		WS("flood", func() WebsocketHandler { return wsFloodHandler{} }),
		WS("panic", func() WebsocketHandler { return wsPanicHandler{} }),
		WS("validated", func() WebsocketHandler { return &wsValidatedHandler{served: &h.served} }, WithLogOption(LogExit)),
		WS("closer", func() WebsocketHandler { return &wsCloserHandler{} }, WithLogOption(LogExit)),
	})

	mux := router.New()
	mux.GET("/ws", wv.Upgrade)
	mux.GET("/ws7", func(ctx *fasthttp.RequestCtx) { wv.UpgradeWithID(ctx, 7) })
	mux.GET("/wsid/{id}", func(ctx *fasthttp.RequestCtx) {
		id, err := strconv.ParseUint(ctx.UserValue("id").(string), 10, 64)
		if err != nil {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			return
		}
		wv.UpgradeWithID(ctx, id)
	})
	l := zerolog.New(h.log)
	if err := v.BuildHandlers(mux, &l); err != nil {
		t.Fatalf("BuildHandlers: %v", err)
	}
	ln := fasthttputil.NewInmemoryListener()
	h.ln = ln
	served := make(chan error, 1)
	go func() { served <- v.serve(ln, mux) }()
	waitServing(t, v)
	t.Cleanup(func() {
		// Closed clients end their connections at once: Shutdown does not
		// wait for unanswered close frames.
		h.mu.Lock()
		for _, c := range h.conns {
			c.Close()
		}
		h.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		v.Shutdown(ctx) //nolint:errcheck
		<-served
	})

	h.client = h.dial()
	return h
}

// dial opens a new WebSocket connection to the harness server.
func (h *wsHarness) dial() *websocket.Client {
	h.t.Helper()
	c, _ := h.dialPath("/ws")
	return c
}

// dialPath opens a new WebSocket connection to path and returns it with its
// network connection.
func (h *wsHarness) dialPath(path string) (*websocket.Client, net.Conn) {
	h.t.Helper()
	conn, err := h.ln.Dial()
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	h.mu.Lock()
	h.conns = append(h.conns, conn)
	h.mu.Unlock()
	c, err := websocket.MakeClient(conn, "http://test"+path)
	if err != nil {
		h.t.Fatalf("websocket client: %v", err)
	}
	return c, conn
}

// send writes a message and returns the reply.
func (h *wsHarness) send(msg string) string {
	h.t.Helper()
	return h.sendOn(h.client, msg)
}

// sendOn writes a message to c and returns the reply. A close frame instead
// of the reply fails the test.
func (h *wsHarness) sendOn(c *websocket.Client, msg string) string {
	h.t.Helper()
	if _, err := c.Write([]byte(msg)); err != nil {
		h.t.Fatalf("write: %v", err)
	}
	fr := websocket.AcquireFrame()
	defer websocket.ReleaseFrame(fr)
	if _, err := c.ReadFrame(fr); err != nil {
		h.t.Fatalf("read: %v", err)
	}
	if fr.IsClose() {
		h.t.Fatalf("connection closed with status %d %q instead of a reply to %s", fr.Status(), fr.Payload(), msg)
	}
	return string(fr.Payload())
}

// readClose reads frames of c until a close frame and returns the payloads
// of the frames before it, and the status and reason of the close frame.
func readClose(t *testing.T, c *websocket.Client) (msgs []string, status websocket.StatusCode, reason string) {
	t.Helper()
	fr := websocket.AcquireFrame()
	defer websocket.ReleaseFrame(fr)
	for {
		fr.Reset()
		if _, err := c.ReadFrame(fr); err != nil {
			t.Fatalf("read: %v (frames so far: %q; want a close frame)", err, msgs)
		}
		if fr.IsClose() {
			return msgs, fr.Status(), string(fr.Payload())
		}
		msgs = append(msgs, string(fr.Payload()))
	}
}

// waitConn waits until the connection cid is registered (its onOpen ran).
func (h *wsHarness) waitConn(cid uint64) {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("connection %d", cid), func() bool {
		h.wv.conns.mu.RLock()
		defer h.wv.conns.mu.RUnlock()
		wc, ok := h.wv.conns.conns[cid]
		return ok && wc.conn != nil
	})
}

// tokenMsg returns an updateAccessToken message with the token tok.
func tokenMsg(tok string) string {
	return `{"path":"updateAccessToken","data":{"mid":"00000000-0000-0000-0000-000000000001","at":"` + tok + `"}}`
}

// waitFor waits until cond holds.
func (h *wsHarness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timeout waiting for %s", what)
}

// waitLog waits until the log contains s.
func (h *wsHarness) waitLog(s string) string {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if out := h.log.String(); strings.Contains(out, s) {
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("log does not contain %s: %s", s, h.log.String())
	return ""
}

// waitMetric waits until path has n metrics.
func (h *wsHarness) waitMetric(path string, n int) ([]int, int) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, alarms := h.metrics.get(path); len(st) >= n {
			return st, alarms
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("no metric for %s", path)
	return nil, 0
}

func TestWS_Integration(t *testing.T) {
	h := newWSHarness(t)

	// Success: the client gets the result, the log gets it masked.
	if got := h.send(`{"path":"secret","data":{"name":"a"}}`); got != `{"name":"a","password":"p1"}` {
		t.Errorf("reply = %s", got)
	}
	out := h.waitLog(`"respBody":{"name":"a","password":"***"}`)
	if strings.Contains(out, "p1") {
		t.Errorf("secret leaked to log: %s", out)
	}
	if st, _ := h.waitMetric("secret", 1); st[0] != 200 {
		t.Errorf("metric status = %v, want 200", st)
	}
	if sz := h.metrics.sizes("secret"); sz[0] != len(`{"name":"a","password":"p1"}`) {
		t.Errorf("metric size = %v, want the size of the reply", sz)
	}

	// Invalid data: 400 VTL-0007 frame.
	if got := h.send(`{"path":"secret","data":{"name":1}}`); !strings.Contains(got, "VTL-0007") {
		t.Errorf("reply to invalid data = %s, want VTL-0007", got)
	}

	// LogSilent: nothing logged on success. Messages of a connection are
	// handled in order, so the line of the next message proves silent is done.
	h.send(`{"path":"silent","data":{"name":"s"}}`)
	h.send(`{"path":"secret","data":{"name":"after-silent"}}`)
	out = h.waitLog(`"name":"after-silent"`)
	if strings.Contains(out, `"path":"silent"`) {
		t.Errorf("LogSilent endpoint logged: %s", out)
	}

	// The token stored by updateAccessToken reaches authFunc with the perms.
	if got := h.send(`{"path":"updateAccessToken","data":{"mid":"00000000-0000-0000-0000-000000000001","at":"tok"}}`); !strings.Contains(got, `"result":"Ok"`) {
		t.Fatalf("updateAccessToken reply = %s", got)
	}
	h.send(`{"path":"perm","data":{"name":"p"}}`)
	h.mu.Lock()
	var permPerms []string
	var permTP any
	for i, p := range h.authPaths {
		if p == "perm" {
			permPerms, permTP = h.authPerms[i], h.authTP[i]
		}
	}
	h.mu.Unlock()
	if len(permPerms) != 1 || permPerms[0] != "orders:read" {
		t.Errorf("authFunc perms = %v, want [orders:read]", permPerms)
	}
	if permTP == nil {
		t.Error("authFunc must see the payload stored by updateAccessToken")
	}

	// A plain error: generic 500 for the client, original error in the log,
	// metric 500 and an alarm.
	got := h.send(`{"path":"fail"}`)
	if strings.Contains(got, "SECRET") || !strings.Contains(got, "VTL-0005") {
		t.Errorf("reply to a plain error = %s, want a generic VTL-0005 error", got)
	}
	h.waitLog("SECRET dsn")

	// A masked field with a value that does not fit: the decode error must
	// not quote the value.
	if got := h.send(`{"path":"pin","data":{"pin":1234.5678}}`); !strings.Contains(got, "VTL-0007") || strings.Contains(got, "1234.5678") {
		t.Errorf("reply to a bad masked value = %s, want VTL-0007 without the value", got)
	}
	if out := h.waitLog(`"field":"pin"`); strings.Contains(out, "1234.5678") {
		t.Errorf("masked ws value leaked to log: %s", out)
	}
	st, alarms := h.waitMetric("fail", 1)
	if st[0] != 500 || alarms != 1 {
		t.Errorf("fail: metric status %v, alarms %d; want 500 and 1", st, alarms)
	}
}

// The Validate() of a WS Inputer input runs after decoding, before Handle: an
// error rejects the message with ErrValidationFailed, as in HTTP.
func TestWS_InputValidated(t *testing.T) {
	h := newWSHarness(t)

	got := h.send(`{"path":"validated","data":{"name":""}}`)
	if !strings.Contains(got, `"code":"VTL-0006","statusCode":400`) {
		t.Errorf("reply to an invalid input = %s, want 400 VTL-0006", got)
	}
	h.waitLog(`"reason":"name is required"`)
	if n := h.served.Load(); n != 0 {
		t.Errorf("handler called %d times for an invalid input, want 0", n)
	}
	if st, _ := h.waitMetric("validated", 1); st[0] != 400 {
		t.Errorf("metric status = %v, want 400", st)
	}

	if got := h.send(`{"path":"validated","data":{"name":"a"}}`); got != `{"name":"a"}` {
		t.Errorf("reply to a valid input = %s", got)
	}
	if n := h.served.Load(); n != 1 {
		t.Errorf("handler called %d times for a valid input, want 1", n)
	}
}

// Errors of the message envelope (invalid JSON, no path, an unknown path) are
// coded, sent through the shared error path and logged; no metric is
// reported for them.
func TestWS_EnvelopeErrors(t *testing.T) {
	h := newWSHarness(t)

	tests := []struct {
		name, msg, wantCode, wantLog string
	}{
		{"invalid JSON", `{"path":`, `"code":"VTL-0007","statusCode":400`, `"msg":"ws message is not valid JSON"`},
		{"no path", `{"data":{}}`, `"code":"VTL-0007","statusCode":400`, `"msg":"ws message has no path"`},
		{"unknown path", `{"path":"nope"}`, `"code":"VTL-0008","statusCode":404`, `"path":"nope"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.send(tt.msg); !strings.Contains(got, tt.wantCode) {
				t.Errorf("reply = %s, want %s", got, tt.wantCode)
			}
			out := h.waitLog(tt.wantLog)
			if !strings.Contains(out, `"method":"WS"`) || !strings.Contains(out, "ws request failed") {
				t.Errorf("envelope error not logged as a ws request failure: %s", out)
			}
		})
	}
	if st, _ := h.metrics.get("nope"); len(st) != 0 {
		t.Errorf("metric reported for an unknown path: %v", st)
	}
}

// A connection opened after WebsocketVatel.Shutdown (e.g. a client that
// reconnects before the listener is closed) is closed at once, without the
// OnOpen and OnClose callbacks.
func TestWS_ConnectionAfterShutdownClosed(t *testing.T) {
	h := newWSHarness(t)
	if got := h.send(`{"path":"secret","data":{"name":"a"}}`); !strings.Contains(got, `"name":"a"`) {
		t.Fatalf("reply = %s", got)
	}
	h.wv.Shutdown()

	c := h.dial()
	fr := websocket.AcquireFrame()
	defer websocket.ReleaseFrame(fr)
	for {
		if _, err := c.ReadFrame(fr); err != nil {
			break // connection closed
		}
		if fr.IsClose() {
			break
		}
	}
	if _, err := c.Write([]byte(`{"path":"secret","data":{"name":"late"}}`)); err == nil {
		if _, err := c.ReadFrame(fr); err == nil && strings.Contains(string(fr.Payload()), "late") {
			t.Error("a connection opened after Shutdown was served")
		}
	}
	if got := h.opened.Load(); got != 1 {
		t.Errorf("OnOpen calls = %d, want 1 (the rejected connection is not reported)", got)
	}
	h.wv.conns.mu.RLock()
	n := len(h.wv.conns.conns)
	h.wv.conns.mu.RUnlock()
	if n > 1 {
		t.Errorf("connections registered after Shutdown: %d", n)
	}
}

// A second connection upgraded with the ID of a live connection is closed at
// once, without OnOpen/OnClose and without serving its messages: it must not
// inherit the token payload of the first connection, nor unregister it.
func TestWS_DuplicateIDRejected(t *testing.T) {
	h := newWSHarness(t)
	a, _ := h.dialPath("/ws7")
	if got := h.sendOn(a, `{"path":"updateAccessToken","data":{"mid":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","at":"tok"}}`); !strings.Contains(got, `"Ok"`) {
		t.Fatalf("updateAccessToken reply = %s", got)
	}

	b, _ := h.dialPath("/ws7")
	_, _ = b.Write([]byte(`{"path":"perm"}`))
	fr := websocket.AcquireFrame()
	defer websocket.ReleaseFrame(fr)
	for {
		if _, err := b.ReadFrame(fr); err != nil || fr.IsClose() {
			break
		}
		t.Errorf("the duplicate connection got a reply: %s", fr.Payload())
	}

	h.mu.Lock()
	for i, p := range h.authPaths {
		if p == "perm" {
			t.Errorf("the duplicate connection was served (token payload %v)", h.authTP[i])
		}
	}
	h.mu.Unlock()
	if got := h.opened.Load(); got != 2 {
		t.Errorf("OnOpen calls = %d, want 2 (the duplicate is not reported)", got)
	}
	if got := h.closed.Load(); got != 0 {
		t.Errorf("OnClose calls = %d, want 0", got)
	}

	// The first connection is still registered and keeps its payload.
	if got := h.sendOn(a, `{"path":"perm","data":{"name":"a"}}`); !strings.Contains(got, `"name":"a"`) {
		t.Fatalf("first connection reply = %s", got)
	}
	h.mu.Lock()
	last := h.authTP[len(h.authTP)-1]
	h.mu.Unlock()
	if last == nil {
		t.Error("the first connection lost its token payload")
	}
}

// Shutdown racing a client close frame (and another Shutdown) must not close
// the connection twice: the websocket library panics on that.
func TestWS_ShutdownRacesClientClose(t *testing.T) {
	for range 10 {
		h := newWSHarness(t)
		// Keep the connection busy, then queue a close frame behind the message.
		if _, err := h.client.Write([]byte(`{"path":"slow"}`)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		fr := websocket.AcquireFrame()
		fr.SetClose()
		fr.SetFin()
		fr.SetStatus(websocket.StatusNone)
		fr.Mask()
		if _, err := h.client.WriteFrame(fr); err != nil {
			t.Fatal(err)
		}
		websocket.ReleaseFrame(fr)
		time.Sleep(20 * time.Millisecond)

		var wg sync.WaitGroup
		for range 2 {
			wg.Go(h.wv.Shutdown)
		}
		wg.Wait()
		h.waitFor("OnClose", func() bool { return h.closed.Load() == 1 })
	}
}

// Shutdown must not block on a connection whose outbound queue is full.
func TestWS_ShutdownFullOutboundQueue(t *testing.T) {
	h := newWSHarness(t)
	c, conn := h.dialPath("/ws")
	defer conn.Close()
	if _, err := c.Write([]byte(`{"path":"flood"}`)); err != nil {
		t.Fatal(err)
	}
	// The client does not read: the handler fills the queue and blocks.
	time.Sleep(100 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		h.wv.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		conn.Close() // unblock the server
		t.Fatal("Shutdown blocked on a full outbound queue")
	}
}

// Shutdown closes an open connection with a "going away" close frame, after
// the message being handled, and reports it to OnClose without an error.
func TestWS_ShutdownGoingAway(t *testing.T) {
	h := newWSHarness(t)
	var closeErr atomic.Value
	h.wv.OnClose(func(_ *websocket.Conn, err error) {
		closeErr.Store(fmt.Sprint(err))
		h.closed.Add(1)
	})
	if _, err := h.client.Write([]byte(`{"path":"slow"}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	h.wv.Shutdown()

	fr := websocket.AcquireFrame()
	defer websocket.ReleaseFrame(fr)
	for {
		if _, err := h.client.ReadFrame(fr); err != nil {
			t.Fatalf("read: %v (want a close frame)", err)
		}
		if fr.IsClose() {
			break
		}
	}
	if fr.Status() != websocket.StatusGoAway {
		t.Errorf("close status = %v, want going away", fr.Status())
	}
	h.waitFor("OnClose", func() bool { return h.closed.Load() == 1 })
	if got := closeErr.Load(); got != "<nil>" {
		t.Errorf("OnClose error = %v, want nil", got)
	}
	h.waitMetric("slow", 1) // the message being handled was finished
}

// A panic in a WS handler is recovered: the client gets a 500 VTL-0005 error
// frame, the panic is logged with its stack, reported to the metrics and the
// Alarmer, and the connection keeps serving messages.
func TestWS_HandlerPanic(t *testing.T) {
	h := newWSHarness(t)

	got := h.send(`{"path":"panic"}`)
	if !strings.Contains(got, "VTL-0005") || strings.Contains(got, "SECRET") {
		t.Errorf("reply = %s, want VTL-0005 without the panic value", got)
	}
	out := h.waitLog(`"panic":"SECRET ws panic"`)
	if !strings.Contains(out, `"stack":"goroutine`) {
		t.Errorf("stack not logged: %s", out)
	}
	if st, alarms := h.waitMetric("panic", 1); st[0] != 500 || alarms != 1 {
		t.Errorf("metric = %v, alarms = %d; want [500] and 1", st, alarms)
	}

	// The connection is still usable.
	if got := h.send(`{"path":"secret","data":{"name":"a"}}`); got != `{"name":"a","password":"p1"}` {
		t.Errorf("reply after a panic = %s", got)
	}
}

// alarms returns the number of Alarmer calls.
func (m *syncMetrics) alarms() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.alarmed)
}

// A panic in the OnOpen callback is recovered: it is logged with the stack,
// the Alarmer is called, the new connection is closed with 1011 and the
// server keeps serving other connections.
func TestWS_OnOpenPanic(t *testing.T) {
	h := newWSHarness(t)
	// OnOpen of the harness client may run after newWSHarness returns.
	h.waitFor("OnOpen", func() bool { return h.opened.Load() == 1 })
	panicking := func() { panic("SECRET open panic") }
	h.openHook.Store(&panicking)
	alarms := h.metrics.alarms()

	c, _ := h.dialPath("/ws")
	fr := websocket.AcquireFrame()
	defer websocket.ReleaseFrame(fr)
	if _, err := c.ReadFrame(fr); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !fr.IsClose() || fr.Status() != websocket.StatusUnexpected {
		t.Errorf("frame = close %v status %v, want close 1011", fr.IsClose(), fr.Status())
	}
	out := h.waitLog(`"panic":"SECRET open panic"`)
	if !strings.Contains(out, `"stack":"goroutine`) || !strings.Contains(out, `"callback":"OnOpen"`) {
		t.Errorf("panic not logged with callback and stack: %s", out)
	}
	h.waitFor("alarm", func() bool { return h.metrics.alarms() >= alarms+1 })
	if n := h.metrics.alarms(); n != alarms+1 {
		h.metrics.mu.Lock()
		t.Errorf("alarms = %d, want %d: %v", n, alarms+1, h.metrics.alarmed)
		h.metrics.mu.Unlock()
	}

	h.openHook.Store(nil)
	if got := h.send(`{"path":"secret","data":{"name":"a"}}`); got != `{"name":"a","password":"p1"}` {
		t.Errorf("reply after an OnOpen panic = %s", got)
	}
}

// A panic in the OnClose callback is recovered: it is logged with the stack,
// the Alarmer is called and the server keeps serving other connections.
func TestWS_OnClosePanic(t *testing.T) {
	h := newWSHarness(t)
	h.waitFor("OnOpen", func() bool { return h.opened.Load() == 1 })
	panicking := func() { panic("SECRET close panic") }
	h.closeHook.Store(&panicking)
	alarms := h.metrics.alarms()

	_, conn := h.dialPath("/ws")
	h.waitFor("OnOpen", func() bool { return h.opened.Load() == 2 })
	conn.Close()
	out := h.waitLog(`"panic":"SECRET close panic"`)
	if !strings.Contains(out, `"stack":"goroutine`) || !strings.Contains(out, `"callback":"OnClose"`) {
		t.Errorf("panic not logged with callback and stack: %s", out)
	}
	h.waitFor("alarm", func() bool { return h.metrics.alarms() >= alarms+1 })
	if n := h.metrics.alarms(); n != alarms+1 {
		h.metrics.mu.Lock()
		t.Errorf("alarms = %d, want %d: %v", n, alarms+1, h.metrics.alarmed)
		h.metrics.mu.Unlock()
	}

	h.closeHook.Store(nil)
	if got := h.send(`{"path":"secret","data":{"name":"a"}}`); got != `{"name":"a","password":"p1"}` {
		t.Errorf("reply after an OnClose panic = %s", got)
	}
}

// Shutdown while a handler floods a client that does not read: the writes of
// the handler after the close frame fail instead of blocking on the full
// outbound queue, so the handler returns and the connection is unregistered
// (OnClose) before Vatel.Shutdown returns.
func TestWS_ShutdownFloodingHandlerFinishes(t *testing.T) {
	h := newWSHarness(t)
	c, conn := h.dialPath("/ws")
	defer conn.Close()
	if _, err := c.Write([]byte(`{"path":"flood"}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the handler fills the queue and blocks

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := h.v.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown = %v, want nil", err)
	}
	// h.client did not send anything: closed as well.
	if n := h.closed.Load(); n != 2 {
		t.Errorf("OnClose calls when Shutdown returned = %d, want 2", n)
	}
	h.waitMetric("flood", 1) // the handler finished
}

// A live connection whose client does not read: once the outbound queue is
// full, a write of the handler fails after the write timeout instead of
// blocking forever, so the handler returns and the connection ends.
func TestWS_NonReadingClientWriteTimeout(t *testing.T) {
	h := newWSHarness(t, WithWsWriteTimeout(200*time.Millisecond))
	c, conn := h.dialPath("/ws")
	defer conn.Close()
	if _, err := c.Write([]byte(`{"path":"flood"}`)); err != nil {
		t.Fatal(err)
	}
	h.waitMetric("flood", 1) // the handler finished
	h.waitFor("OnClose", func() bool { return h.closed.Load() == 1 })
}

// ── security context: one connection, one (user, organization) ──────────────

// ctxPayload is a token payload with a security context (user, organization)
// and an optional expiry.
type ctxPayload struct {
	user, org int
	exp       time.Time
}

func (p *ctxPayload) Perms() []byte        { return []byte{1} }
func (p *ctxPayload) Debug() bool          { return false }
func (p *ctxPayload) ExpiresAt() time.Time { return p.exp }

// ctxDecoder decodes tokens "user/org", or "user/org/ttl" with a Go duration
// ttl: the payload expires ttl after decoding (a negative ttl: it has
// expired).
type ctxDecoder struct{}

func (ctxDecoder) Decode(b []byte) (Tokener, error) {
	parts := strings.Split(string(b), "/")
	if len(parts) < 2 {
		return nil, stderrors.New("malformed token")
	}
	p := &ctxPayload{}
	var err error
	if p.user, err = strconv.Atoi(parts[0]); err != nil {
		return nil, err
	}
	if p.org, err = strconv.Atoi(parts[1]); err != nil {
		return nil, err
	}
	if len(parts) > 2 {
		ttl, err := time.ParseDuration(parts[2])
		if err != nil {
			return nil, err
		}
		p.exp = time.Now().Add(ttl)
	}
	return &mockTokener{payload: p}, nil
}

// sameSecurityContext is the OnTokenUpdate policy of one security context
// per connection: the first token is accepted; a later one must have the same
// user and organization.
func sameSecurityContext(_ uint64, prev, next AuthPayload) error {
	if prev == nil {
		return nil
	}
	p, n := prev.(*ctxPayload), next.(*ctxPayload)
	switch {
	case p.user != n.user:
		return NewWsCloseError(WsStatusForbidden, "user changed", nil)
	case p.org != n.org:
		return NewWsCloseError(WsStatusContextChanged, "organization changed", nil)
	}
	return nil
}

// A handler error with a WsCloseError: the client gets the error reply for
// the message, then the close frame with the code and reason; the next
// message is not served. The reply comes from Err when it has an
// *errors.Error, otherwise from the code. No alarm: it is not a 5xx.
func TestWS_CloseErrorFromHandler(t *testing.T) {
	tests := []struct {
		name, data, wantReply string
		wantCode              websocket.StatusCode
		wantStatus            int
	}{
		{"4001", `{"code":4001,"reason":"auth required"}`, `"code":"VTL-0004","statusCode":401`, WsStatusAuthRequired, 401},
		{"4003", `{"code":4003,"reason":"forbidden"}`, `"code":"VTL-0003","statusCode":403`, WsStatusForbidden, 403},
		{"4009", `{"code":4009,"reason":"context changed"}`, `"code":"VTL-0009","statusCode":409`, WsStatusContextChanged, 409},
		{"other 4xxx", `{"code":4100,"reason":"custom"}`, `"statusCode":400`, 4100, 400},
		{"Err with *errors.Error", `{"code":4003,"reason":"r","coded":true}`, `"code":"VTL-0006","statusCode":400`, WsStatusForbidden, 400},
	}
	h := newWSHarness(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := h.dialPath("/ws")
			if _, err := c.Write([]byte(`{"path":"closer","data":` + tt.data + `}`)); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Write([]byte(`{"path":"secret","data":{"name":"late"}}`)); err != nil {
				t.Fatal(err)
			}
			msgs, code, _ := readClose(t, c)
			if len(msgs) != 1 || !strings.Contains(msgs[0], tt.wantReply) {
				t.Errorf("frames before close = %q, want one error reply with %s", msgs, tt.wantReply)
			}
			if code != tt.wantCode {
				t.Errorf("close status = %d, want %d", code, tt.wantCode)
			}
		})
	}
	st, alarms := h.waitMetric("closer", len(tests))
	for i, tt := range tests {
		if st[i] != tt.wantStatus {
			t.Errorf("metric status %d = %d, want %d", i, st[i], tt.wantStatus)
		}
	}
	if alarms != 0 {
		t.Errorf("alarms = %d, want 0 for WsCloseError", alarms)
	}
	if st, _ := h.metrics.get("secret"); len(st) != 0 {
		t.Errorf("a message after the closing one was served: %v", st)
	}
}

// A WsCloseError from the WithWsAuthFunc function closes the connection after
// the error reply as well.
func TestWS_CloseErrorFromAuthFunc(t *testing.T) {
	h := newWSHarness(t)
	revoked := func(path string) error {
		if path == "perm" {
			return NewWsCloseError(WsStatusAuthRequired, "token revoked", nil)
		}
		return nil
	}
	h.authHook.Store(&revoked)
	if _, err := h.client.Write([]byte(`{"path":"perm","data":{"name":"a"}}`)); err != nil {
		t.Fatal(err)
	}
	msgs, code, reason := readClose(t, h.client)
	if len(msgs) != 1 || !strings.Contains(msgs[0], `"code":"VTL-0004","statusCode":401`) {
		t.Errorf("frames before close = %q, want a 401 VTL-0004 reply", msgs)
	}
	if code != WsStatusAuthRequired || reason != "token revoked" {
		t.Errorf("close = %d %q, want 4001 \"token revoked\"", code, reason)
	}
}

// OnTokenUpdate gets the previous and the new payload (prev == nil for the
// first token). A token of the same context is accepted; a token of another
// organization closes the connection with 4009 and of another user with
// 4003, after the error reply, and is not stored.
func TestWS_SecurityContextPolicy(t *testing.T) {
	h := newWSHarness(t, WithTokenDecoder(ctxDecoder{}))
	type call struct {
		cid        uint64
		prev, next AuthPayload
	}
	var mu sync.Mutex
	var calls []call
	h.wv.OnTokenUpdate(func(cid uint64, prev, next AuthPayload) error {
		mu.Lock()
		calls = append(calls, call{cid, prev, next})
		mu.Unlock()
		return sameSecurityContext(cid, prev, next)
	})
	lastCall := func() call {
		mu.Lock()
		defer mu.Unlock()
		return calls[len(calls)-1]
	}

	tests := []struct {
		name, second, wantReply string
		wantClose               websocket.StatusCode
	}{
		{"same context", "1/10", `"result":"Ok"`, 0},
		{"other organization", "1/20", `"code":"VTL-0009","statusCode":409`, WsStatusContextChanged},
		{"other user", "2/10", `"code":"VTL-0003","statusCode":403`, WsStatusForbidden},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cid := uint64(100 + i)
			c, _ := h.dialPath("/wsid/" + strconv.FormatUint(cid, 10))
			if got := h.sendOn(c, tokenMsg("1/10")); !strings.Contains(got, `"result":"Ok"`) {
				t.Fatalf("first token reply = %s", got)
			}
			first := lastCall()
			if first.cid != cid || first.prev != nil {
				t.Errorf("first call: cid %d prev %v; want cid %d, prev nil", first.cid, first.prev, cid)
			}
			stored, _ := h.wv.conns.payload(cid)
			if stored != first.next {
				t.Fatalf("stored payload %v, want the first token's %v", stored, first.next)
			}

			if got := h.sendOn(c, tokenMsg(tt.second)); !strings.Contains(got, tt.wantReply) {
				t.Errorf("second token reply = %s, want %s", got, tt.wantReply)
			}
			second := lastCall()
			if second.prev != first.next {
				t.Errorf("second call: prev %v, want the stored payload %v", second.prev, first.next)
			}
			if tt.wantClose == 0 {
				if tp, _ := h.wv.conns.payload(cid); tp != second.next {
					t.Errorf("stored payload %v, want the accepted %v", tp, second.next)
				}
				h.sendOn(c, `{"path":"secret","data":{"name":"still open"}}`)
				return
			}
			if tp, ok := h.wv.conns.payload(cid); ok && tp != first.next {
				t.Errorf("rejected payload stored: %v", tp)
			}
			msgs, code, _ := readClose(t, c)
			if len(msgs) != 0 || code != tt.wantClose {
				t.Errorf("frames %q, close status %d; want close %d", msgs, code, tt.wantClose)
			}
		})
	}
}

// CloseConn closes a live connection by ID with the code and reason and
// reports true; for an unknown ID, or a connection already being closed, it
// reports false.
func TestWS_CloseConn(t *testing.T) {
	h := newWSHarness(t)
	c, _ := h.dialPath("/wsid/42")
	h.waitConn(42)

	if h.wv.CloseConn(99, WsStatusForbidden, "x") {
		t.Error("CloseConn of an unknown ID = true, want false")
	}
	if !h.wv.CloseConn(42, WsStatusForbidden, "removed from organization") {
		t.Fatal("CloseConn of a live connection = false, want true")
	}
	if h.wv.CloseConn(42, WsStatusForbidden, "again") {
		t.Error("CloseConn of a closing connection = true, want false")
	}
	msgs, code, reason := readClose(t, c)
	if len(msgs) != 0 || code != WsStatusForbidden || reason != "removed from organization" {
		t.Errorf("frames %q, close %d %q; want close 4003 \"removed from organization\"", msgs, code, reason)
	}
	h.waitFor("OnClose", func() bool { return h.closed.Load() == 1 })
	if h.wv.CloseConn(42, WsStatusForbidden, "closed") {
		t.Error("CloseConn of a closed connection = true, want false")
	}
	if got := h.send(`{"path":"secret","data":{"name":"a"}}`); !strings.Contains(got, `"name":"a"`) {
		t.Errorf("other connection reply = %s", got)
	}
}

// The close reason is cut to 123 bytes (the close frame payload is at most
// 125 bytes with the code) on a UTF-8 character boundary.
func TestWS_CloseReasonTruncated(t *testing.T) {
	h := newWSHarness(t)
	c, _ := h.dialPath("/wsid/43")
	h.waitConn(43)
	long := strings.Repeat("я", 100) // 200 bytes
	if !h.wv.CloseConn(43, WsStatusForbidden, long) {
		t.Fatal("CloseConn = false")
	}
	_, code, reason := readClose(t, c)
	if code != WsStatusForbidden {
		t.Errorf("close status = %d, want 4003", code)
	}
	if reason != strings.Repeat("я", 61) || !utf8.ValidString(reason) {
		t.Errorf("reason = %q (%d bytes), want 61 runes (122 bytes)", reason, len(reason))
	}
}

// VisitConns walks the live connections with their payloads; its callback may
// call CloseConn (no deadlock) and stops the walk by returning false.
func TestWS_VisitConnsCloseConn(t *testing.T) {
	h := newWSHarness(t, WithTokenDecoder(ctxDecoder{}))
	conns := map[uint64]*websocket.Client{}
	for cid, tok := range map[uint64]string{101: "1/10", 102: "2/10", 103: "3/20"} {
		c, _ := h.dialPath("/wsid/" + strconv.FormatUint(cid, 10))
		if got := h.sendOn(c, tokenMsg(tok)); !strings.Contains(got, `"Ok"`) {
			t.Fatalf("token reply = %s", got)
		}
		conns[cid] = c
	}

	var visited []uint64
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.wv.VisitConns(func(cid uint64, tp AuthPayload) bool {
			visited = append(visited, cid)
			if p, ok := tp.(*ctxPayload); ok && p.org == 10 {
				if !h.wv.CloseConn(cid, WsStatusForbidden, "organization deleted") {
					t.Errorf("CloseConn(%d) = false", cid)
				}
			}
			return true
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("VisitConns with CloseConn in its callback deadlocked")
	}
	for _, cid := range []uint64{101, 102, 103} {
		if !slices.Contains(visited, cid) {
			t.Errorf("connection %d not visited: %v", cid, visited)
		}
	}
	for _, cid := range []uint64{101, 102} {
		if _, code, _ := readClose(t, conns[cid]); code != WsStatusForbidden {
			t.Errorf("connection %d: close status %d, want 4003", cid, code)
		}
	}
	h.sendOn(conns[103], `{"path":"secret","data":{"name":"org 20"}}`)

	// Closing connections are not visited; false stops the walk.
	n := 0
	h.wv.VisitConns(func(cid uint64, _ AuthPayload) bool {
		n++
		if cid == 101 || cid == 102 {
			t.Errorf("closing connection %d visited", cid)
		}
		return false
	})
	if n != 1 {
		t.Errorf("callback calls after returning false = %d, want 1", n)
	}
}

// A payload implementing Expirer closes its connection with 4001 when it
// expires; a token update before that moves the expiry.
func TestWS_TokenExpiry(t *testing.T) {
	h := newWSHarness(t, WithTokenDecoder(ctxDecoder{}))

	c, _ := h.dialPath("/wsid/201")
	start := time.Now()
	if got := h.sendOn(c, tokenMsg("1/10/200ms")); !strings.Contains(got, `"Ok"`) {
		t.Fatalf("token reply = %s", got)
	}
	_, code, reason := readClose(t, c)
	if code != WsStatusAuthRequired || reason != "token expired" {
		t.Errorf("close = %d %q, want 4001 \"token expired\"", code, reason)
	}
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Errorf("closed %v after the token, before it expired", d)
	}

	c2, _ := h.dialPath("/wsid/202")
	firstExp := time.Now().Add(300 * time.Millisecond)
	h.sendOn(c2, tokenMsg("1/10/300ms"))
	h.sendOn(c2, tokenMsg("1/10/1500ms"))
	time.Sleep(time.Until(firstExp.Add(300 * time.Millisecond)))
	h.sendOn(c2, `{"path":"secret","data":{"name":"after the first expiry"}}`)
	if _, code, _ := readClose(t, c2); code != WsStatusAuthRequired {
		t.Errorf("close status = %d, want 4001", code)
	}
	if d := time.Since(firstExp); d < time.Second {
		t.Errorf("closed %v after the first expiry, want after the second", d)
	}
}

// WithWsAuthTimeout: a connection without a stored payload is closed with
// 4001 after the timeout (a rejected token does not count); a connection
// that got a token in time stays open.
func TestWS_AuthTimeout(t *testing.T) {
	h := newWSHarness(t, WithTokenDecoder(ctxDecoder{}), WithWsAuthTimeout(200*time.Millisecond))

	start := time.Now()
	authed, _ := h.dialPath("/wsid/301")
	if got := h.sendOn(authed, tokenMsg("1/10")); !strings.Contains(got, `"Ok"`) {
		t.Fatalf("token reply = %s", got)
	}
	expired, _ := h.dialPath("/wsid/302")
	if got := h.sendOn(expired, tokenMsg("1/10/-1s")); !strings.Contains(got, `"code":"VTL-0004","statusCode":401`) {
		t.Errorf("expired token reply = %s, want 401 VTL-0004", got)
	}

	for name, c := range map[string]*websocket.Client{"silent": h.client, "expired token": expired} {
		if _, code, reason := readClose(t, c); code != WsStatusAuthRequired || reason != "authentication timeout" {
			t.Errorf("%s: close = %d %q, want 4001 \"authentication timeout\"", name, code, reason)
		}
	}
	time.Sleep(time.Until(start.Add(500 * time.Millisecond)))
	h.sendOn(authed, `{"path":"secret","data":{"name":"authenticated"}}`)
}

// Shutdown and the close of a connection stop its timer.
func TestWS_ShutdownStopsTimers(t *testing.T) {
	h := newWSHarness(t, WithTokenDecoder(ctxDecoder{}), WithWsAuthTimeout(time.Hour))
	h.dialPath("/wsid/401")
	h.waitConn(401)
	timers := func() (n int) {
		h.wv.conns.mu.RLock()
		defer h.wv.conns.mu.RUnlock()
		for _, wc := range h.wv.conns.conns {
			if wc.timer != nil {
				n++
			}
		}
		return n
	}
	if n := timers(); n == 0 {
		t.Fatal("no auth timer armed")
	}
	h.wv.Shutdown()
	if n := timers(); n != 0 {
		t.Errorf("timers armed after Shutdown: %d", n)
	}
}

// A token stored for an ID (UpdateAccessToken) before its connection opened
// that has expired by then authorizes nothing: the connection opens without a
// payload, its first token is accepted as the first one (prev == nil), and
// without one the auth timeout closes it with 4001.
func TestWS_PreStoredExpiredPayload(t *testing.T) {
	h := newWSHarness(t, WithTokenDecoder(ctxDecoder{}), WithWsAuthTimeout(time.Second))
	h.wv.OnTokenUpdate(sameSecurityContext)
	for _, cid := range []uint64{501, 502} {
		if err := h.wv.UpdateAccessToken(cid, "1/10/50ms"); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)

	c, _ := h.dialPath("/wsid/501")
	h.sendOn(c, `{"path":"perm","data":{"name":"a"}}`)
	h.mu.Lock()
	tp := h.authTP[len(h.authTP)-1]
	h.mu.Unlock()
	if tp != nil {
		t.Errorf("message authorized with the expired payload %v", tp)
	}
	// Another organization than the expired payload: accepted as the first token.
	if got := h.sendOn(c, tokenMsg("1/20")); !strings.Contains(got, `"result":"Ok"`) {
		t.Errorf("first token reply = %s, want Ok", got)
	}

	c2, _ := h.dialPath("/wsid/502")
	if _, code, reason := readClose(t, c2); code != WsStatusAuthRequired || reason != "authentication timeout" {
		t.Errorf("close = %d %q, want 4001 \"authentication timeout\"", code, reason)
	}
	h.sendOn(c, `{"path":"secret","data":{"name":"still open"}}`)
}

// expireWithLateTimer stores the token tok, which expires after ttl, for the
// connection cid opened on c, stops its expiry timer, as if it fired late,
// and waits until the payload has expired.
func (h *wsHarness) expireWithLateTimer(c *websocket.Client, cid uint64, ttl time.Duration) AuthPayload {
	h.t.Helper()
	if got := h.sendOn(c, tokenMsg("1/10/"+ttl.String())); !strings.Contains(got, `"result":"Ok"`) {
		h.t.Fatalf("token reply = %s", got)
	}
	h.wv.conns.mu.Lock()
	wc := h.wv.conns.conns[cid]
	wc.stopTimer()
	tp, exp := wc.tp, wc.exp
	h.wv.conns.mu.Unlock()
	time.Sleep(time.Until(exp.Add(50 * time.Millisecond)))
	return tp
}

// A token update on a connection whose payload has expired, before its
// expiry timer fired, is not served: the connection is closed with 4001
// "token expired", OnTokenUpdate is not called and the payload is not
// replaced, so the connection neither survives its expiry nor switches its
// security context.
func TestWS_TokenUpdateAfterExpiryBeforeTimer(t *testing.T) {
	h := newWSHarness(t, WithTokenDecoder(ctxDecoder{}))
	var calls atomic.Int32
	h.wv.OnTokenUpdate(func(cid uint64, prev, next AuthPayload) error {
		calls.Add(1)
		return sameSecurityContext(cid, prev, next)
	})
	tests := []struct{ name, tok string }{
		{"other organization", "1/20"},
		{"same context", "1/10"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cid := uint64(600 + i)
			c, _ := h.dialPath("/wsid/" + strconv.FormatUint(cid, 10))
			tp := h.expireWithLateTimer(c, cid, 100*time.Millisecond)
			before := calls.Load()
			if _, err := c.Write([]byte(tokenMsg(tt.tok))); err != nil {
				t.Fatal(err)
			}
			msgs, code, reason := readClose(t, c)
			if len(msgs) != 0 || code != WsStatusAuthRequired || reason != "token expired" {
				t.Errorf("frames %q, close %d %q; want no reply, 4001 \"token expired\"", msgs, code, reason)
			}
			if n := calls.Load() - before; n != 0 {
				t.Errorf("OnTokenUpdate calls after expiry = %d, want 0", n)
			}
			if got, _ := h.wv.conns.payload(cid); got != nil && got != tp {
				t.Errorf("payload replaced after expiry: %v", got)
			}
		})
	}

	// A direct UpdateAccessToken call gets ErrWsTokenConflict and closes the
	// connection the same way.
	c, _ := h.dialPath("/wsid/610")
	tp := h.expireWithLateTimer(c, 610, 100*time.Millisecond)
	before := calls.Load()
	if err := h.wv.UpdateAccessToken(610, "1/20"); !stderrors.Is(err, ErrWsTokenConflict) {
		t.Errorf("UpdateAccessToken after expiry = %v, want ErrWsTokenConflict", err)
	}
	if n := calls.Load() - before; n != 0 {
		t.Errorf("OnTokenUpdate calls after expiry = %d, want 0", n)
	}
	if got, _ := h.wv.conns.payload(610); got != nil && got != tp {
		t.Errorf("payload replaced after expiry: %v", got)
	}
	if _, code, reason := readClose(t, c); code != WsStatusAuthRequired || reason != "token expired" {
		t.Errorf("close %d %q, want 4001 \"token expired\"", code, reason)
	}
}

// A message on a connection whose payload has expired, before its expiry
// timer fired, is not served: the connection is closed with 4001 "token
// expired". VisitConns leaves such a connection out and closes it as well.
func TestWS_MessageAfterExpiryBeforeTimer(t *testing.T) {
	h := newWSHarness(t, WithTokenDecoder(ctxDecoder{}))
	c, _ := h.dialPath("/wsid/620")
	h.expireWithLateTimer(c, 620, 100*time.Millisecond)
	if _, err := c.Write([]byte(`{"path":"secret","data":{"name":"after expiry"}}`)); err != nil {
		t.Fatal(err)
	}
	msgs, code, reason := readClose(t, c)
	if len(msgs) != 0 || code != WsStatusAuthRequired || reason != "token expired" {
		t.Errorf("frames %q, close %d %q; want no reply, 4001 \"token expired\"", msgs, code, reason)
	}
	if st, _ := h.metrics.get("secret"); len(st) != 0 {
		t.Errorf("a message after the expiry was served: %v", st)
	}

	c2, _ := h.dialPath("/wsid/621")
	h.expireWithLateTimer(c2, 621, 100*time.Millisecond)
	h.wv.VisitConns(func(cid uint64, _ AuthPayload) bool {
		if cid == 621 {
			t.Error("connection with an expired payload visited")
		}
		return true
	})
	if _, code, reason := readClose(t, c2); code != WsStatusAuthRequired || reason != "token expired" {
		t.Errorf("close %d %q, want 4001 \"token expired\"", code, reason)
	}
}
