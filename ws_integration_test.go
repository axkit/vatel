package vatel

import (
	"bytes"
	"context"
	stderrors "errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fasthttp/router"
	"github.com/regorov/websocket"
	"github.com/rs/zerolog"
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
	alarmed []error
}

func (m *syncMetrics) ReportMetric(_, path string, statusCode int, _ float64, _ int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.status == nil {
		m.status = make(map[string][]int)
	}
	m.status[path] = append(m.status[path], statusCode)
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

type wsFailHandler struct{}

func (wsFailHandler) Handle(WebsocketContext) error { return stderrors.New("SECRET dsn") }

type wsHarness struct {
	t       *testing.T
	client  *websocket.Client
	log     *syncBuffer
	metrics *syncMetrics

	mu        sync.Mutex
	authPaths []string
	authPerms [][]string
	authTP    []any
}

func newWSHarness(t *testing.T) *wsHarness {
	t.Helper()
	h := &wsHarness{t: t, log: &syncBuffer{}, metrics: &syncMetrics{}}

	wv := NewWebsocketVatel(
		WithTokenDecoder(&mockDecoder{token: &mockTokener{payload: &mockPayload{perms: []byte{1}}}}),
		WithWsAuthFunc(func(ctx WebsocketContext, path string, perms []string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.authPaths = append(h.authPaths, path)
			h.authPerms = append(h.authPerms, perms)
			h.authTP = append(h.authTP, ctx.TokenPayload())
			return nil
		}),
	)
	v := NewVatel(WithWebsocketSupport(wv), WithMetricReporter(h.metrics), WithAlarmer(h.metrics))
	v.Add(Endpoints{
		WS("secret", func() WebsocketHandler { return &wsSecretHandler{} }, WithLogOption(LogExit|LogRespBody)),
		WS("silent", func() WebsocketHandler { return &wsSecretHandler{} }, WithLogOption(LogSilent)),
		WS("perm", func() WebsocketHandler { return &wsSecretHandler{} }, Perms("orders:read"), WithLogOption(LogExit)),
		WS("fail", func() WebsocketHandler { return wsFailHandler{} }, WithLogOption(LogExit)),
		WS("pin", func() WebsocketHandler { return &wsPINHandler{} }, WithLogOption(LogExit)),
	})

	mux := router.New()
	mux.GET("/ws", wv.Upgrade)
	l := zerolog.New(h.log)
	if err := v.BuildHandlers(mux, &l); err != nil {
		t.Fatalf("BuildHandlers: %v", err)
	}
	ln := fasthttputil.NewInmemoryListener()
	served := make(chan error, 1)
	go func() { served <- v.serve(ln, mux) }()
	waitServing(t, v)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		v.Shutdown(ctx) //nolint:errcheck
		<-served
	})

	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	c, err := websocket.MakeClient(conn, "http://test/ws")
	if err != nil {
		t.Fatalf("websocket client: %v", err)
	}
	h.client = c
	return h
}

// send writes a message and returns the reply.
func (h *wsHarness) send(msg string) string {
	h.t.Helper()
	if _, err := h.client.Write([]byte(msg)); err != nil {
		h.t.Fatalf("write: %v", err)
	}
	fr := websocket.AcquireFrame()
	defer websocket.ReleaseFrame(fr)
	if _, err := h.client.ReadFrame(fr); err != nil {
		h.t.Fatalf("read: %v", err)
	}
	return string(fr.Payload())
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
