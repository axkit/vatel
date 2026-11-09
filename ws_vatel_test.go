package vatel

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/axkit/errors"
	"github.com/fasthttp/router"
	"github.com/regorov/websocket"
	"github.com/rs/zerolog"
)

// payload returns the token payload stored for the connection cid, if any.
// Tests only: requests read the payload of their connection via serving.
func (cs *wsConns) payload(cid uint64) (AuthPayload, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if wc, ok := cs.conns[cid]; ok && wc.tp != nil {
		return wc.tp, true
	}
	return nil, false
}

// setPayload stores tp for the connection cid unconditionally. Tests only:
// UpdateAccessToken stores a payload with storePayload.
func (cs *wsConns) setPayload(cid uint64, tp AuthPayload) {
	wc, _, ver, _ := cs.lookup(cid)
	cs.storePayload(cid, wc, ver, tp, time.Time{})
}

func TestDecodeWSInput_Invalid(t *testing.T) {
	type input struct {
		Name string `json:"name"`
	}
	cases := map[string][]byte{
		"malformed":  []byte(`{"name":`),
		"wrong type": []byte(`{"name":42}`),
		"empty":      nil,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			var in input
			err := decodeWSInput(data, &in, false)
			if err == nil {
				t.Fatal("expected error")
			}
			if !errors.Is(err, ErrInvalidRequestBody) {
				t.Errorf("want ErrInvalidRequestBody, got %v", err)
			}
			se := errors.Serialize(err, errors.WithAttributes(errors.ServerOutputFormat))
			if se.StatusCode != 400 {
				t.Errorf("want status 400, got %d", se.StatusCode)
			}
			if se.Code != "VTL-0007" {
				t.Errorf("want code VTL-0007, got %q", se.Code)
			}
		})
	}
}

func TestDecodeWSInput_Valid(t *testing.T) {
	type input struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	var in input
	if err := decodeWSInput([]byte(`{"name":"bob","age":7}`), &in, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if in.Name != "bob" || in.Age != 7 {
		t.Errorf("want {bob 7}, got %+v", in)
	}
}

func TestWsWriteErrorResponse_LogSilent(t *testing.T) {
	cases := []struct {
		name    string
		lo      LogOption
		err     error
		wantLog bool
	}{
		{"silent 4xx", LogSilent, ErrValidationFailed.New(), false},
		{"silent 4xx with other bits", LogSilent | LogExit, ErrInvalidRequestBody.New(), false},
		{"silent 5xx", LogSilent, ErrInternalError.New(), true},
		{"silent plain error is 5xx", LogSilent, stderrors.New("boom"), true},
		{"silent error without status is 5xx", LogSilent, errors.Template("no status").New(), true},
		{"not silent 4xx", LogExit, ErrValidationFailed.New(), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logBuf, out bytes.Buffer
			zc := zerolog.New(&logBuf).With()
			e := &Endpoint{EndpointMeta: EndpointMeta{Method: "WS", Path: "x"}}
			compileCommon(NewVatel(), e)

			e.wsWriteErrorResponse(&out, c.lo, false, zc, time.Now(), c.err)

			if out.Len() == 0 {
				t.Error("error must be sent to the client")
			}
			if got := logBuf.Len() != 0; got != c.wantLog {
				t.Errorf("logged = %v, want %v (%s)", got, c.wantLog, logBuf.String())
			}
		})
	}
}

// ── build ─────────────────────────────────────────────────────────────────────

func wsCtrl() WebsocketHandler { return &plainWsHandler{} }

func TestBuildHandlers_WSErrors(t *testing.T) {
	cases := []struct {
		name string
		opts []func(*Option)
		eps  Endpoints
	}{
		{
			name: "no websocket support",
			eps:  Endpoints{WS("ping", wsCtrl)},
		},
		{
			name: "duplicate WS path",
			opts: []func(*Option){WithWebsocketSupport(NewWebsocketVatel())},
			eps:  Endpoints{WS("ping", wsCtrl), WS("ping", wsCtrl)},
		},
		{
			name: "perms without auth func",
			opts: []func(*Option){WithWebsocketSupport(NewWebsocketVatel())},
			eps:  Endpoints{WS("ping", wsCtrl, Perms("a"))},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := NewVatel(c.opts...)
			v.Add(c.eps)
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("BuildHandlers must not panic: %v", r)
					}
				}()
				err = v.BuildHandlers(router.New(), discardLogger())
			}()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), "ping") {
				t.Errorf("error must name the endpoint path: %v", err)
			}
		})
		t.Run(c.name+"/must panics", func(t *testing.T) {
			v := NewVatel(c.opts...)
			v.Add(c.eps)
			defer func() {
				if recover() == nil {
					t.Error("MustBuildHandlers must panic")
				}
			}()
			v.MustBuildHandlers(router.New(), discardLogger())
		})
	}
}

func TestBuildHandlers_AfterFailedBuild(t *testing.T) {
	v := NewVatel()
	// "/items" sorts before "ping", so it is registered before the WS failure.
	v.Add(Endpoints{GET("/items", func() Handler { return &plainHandler{} }), WS("ping", wsCtrl)})
	mux := router.New()

	if err := v.BuildHandlers(mux, discardLogger()); err == nil {
		t.Fatal("first build must fail")
	}

	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("second build must not register routes again: %v", r)
			}
		}()
		err = v.BuildHandlers(mux, discardLogger())
	}()
	if err == nil || !strings.Contains(err.Error(), "previous build failed") {
		t.Fatalf("want 'previous build failed' error, got %v", err)
	}
	// A regression would start serving and block: run it with a timeout.
	errc := make(chan error, 1)
	go func() { errc <- v.ListenAndServe("127.0.0.1:0", mux, discardLogger()) }()
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "previous build failed") {
			t.Fatalf("ListenAndServe after failed build: want 'previous build failed', got %v", err)
		}
	case <-time.After(5 * time.Second):
		v.Shutdown(context.Background()) //nolint:errcheck
		t.Fatal("ListenAndServe after failed build started the server")
	}
}

// ── OnTokenUpdate ─────────────────────────────────────────────────────────────

func TestOnTokenUpdate(t *testing.T) {
	first, second := &mockPayload{perms: []byte{1}}, &mockPayload{perms: []byte{2}}
	dec := &mockDecoder{token: &mockTokener{payload: first}}
	wv := NewWebsocketVatel(WithTokenDecoder(dec))

	var gotCID uint64
	var gotPrev, gotNext AuthPayload
	var stored bool
	wv.OnTokenUpdate(func(cid uint64, prev, next AuthPayload) error {
		gotCID, gotPrev, gotNext = cid, prev, next
		_, stored = wv.conns.payload(cid)
		return nil
	})
	if err := wv.UpdateAccessToken(42, "token"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotCID != 42 || gotPrev != nil || gotNext != AuthPayload(first) {
		t.Errorf("first callback got cid=%d prev=%v next=%v; want 42, nil, the first payload", gotCID, gotPrev, gotNext)
	}
	if stored {
		t.Error("the callback must run before the payload is stored")
	}
	if tp, ok := wv.conns.payload(42); !ok || tp != AuthPayload(first) {
		t.Errorf("stored payload = %v, %v; want the accepted payload", tp, ok)
	}

	dec.token = &mockTokener{payload: second}
	if err := wv.UpdateAccessToken(42, "token"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPrev != AuthPayload(first) || gotNext != AuthPayload(second) {
		t.Errorf("second callback got prev=%v next=%v; want the first and the second payload", gotPrev, gotNext)
	}

	// A rejected token is not stored: the connection keeps its payload.
	errCb := stderrors.New("rejected")
	wv.OnTokenUpdate(func(uint64, AuthPayload, AuthPayload) error { return errCb })
	dec.token = &mockTokener{payload: first}
	if err := wv.UpdateAccessToken(42, "token"); !stderrors.Is(err, errCb) {
		t.Errorf("want callback error, got %v", err)
	}
	if tp, _ := wv.conns.payload(42); tp != AuthPayload(second) {
		t.Errorf("stored payload = %v, want the previous payload after a rejection", tp)
	}

	// A WsCloseError from the callback is returned as is; nothing is stored.
	wv.OnTokenUpdate(func(uint64, AuthPayload, AuthPayload) error {
		return NewWsCloseError(WsStatusContextChanged, "organization changed", nil)
	})
	var wce *WsCloseError
	if err := wv.UpdateAccessToken(42, "token"); !stderrors.As(err, &wce) || wce.Code != WsStatusContextChanged {
		t.Errorf("want the WsCloseError 4009, got %v", err)
	}
	if tp, _ := wv.conns.payload(42); tp != AuthPayload(second) {
		t.Errorf("stored payload = %v, want the previous payload after a WsCloseError", tp)
	}
}

// A payload that has already expired is rejected with ErrUnauthorized before
// the callback; the previous payload stays.
func TestUpdateAccessToken_ExpiredPayload(t *testing.T) {
	wv := NewWebsocketVatel(WithTokenDecoder(ctxDecoder{}))
	calls := 0
	wv.OnTokenUpdate(func(uint64, AuthPayload, AuthPayload) error { calls++; return nil })
	if err := wv.UpdateAccessToken(1, "1/10"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prev, _ := wv.conns.payload(1)

	err := wv.UpdateAccessToken(1, "1/10/-1s")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("want ErrUnauthorized, got %v", err)
	}
	var wce *WsCloseError
	if stderrors.As(err, &wce) {
		t.Error("an expired token must not close the connection")
	}
	if calls != 1 {
		t.Errorf("callback calls = %d, want 1 (not called for the expired token)", calls)
	}
	if tp, _ := wv.conns.payload(1); tp != prev {
		t.Errorf("stored payload = %v, want the previous one", tp)
	}
}

// Concurrent UpdateAccessToken calls for one connection: when the payload
// changed while the callback ran, the later store fails with
// ErrWsTokenConflict (409, VTL-0010) instead of overwriting it.
func TestUpdateAccessToken_Concurrent(t *testing.T) {
	wv := NewWebsocketVatel(WithTokenDecoder(ctxDecoder{}))
	if err := wv.UpdateAccessToken(5, "1/1"); err != nil {
		t.Fatal(err)
	}
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	wv.OnTokenUpdate(func(uint64, AuthPayload, AuthPayload) error {
		arrived <- struct{}{}
		<-release
		return nil
	})
	errs := make(chan error, 2)
	for _, tok := range []string{"1/10", "1/11"} {
		go func() { errs <- wv.UpdateAccessToken(5, tok) }()
	}
	<-arrived
	<-arrived
	close(release)
	e1, e2 := <-errs, <-errs
	if (e1 == nil) == (e2 == nil) {
		t.Fatalf("errors = %v, %v; want exactly one nil", e1, e2)
	}
	conflict := e1
	if conflict == nil {
		conflict = e2
	}
	if !errors.Is(conflict, ErrWsTokenConflict) {
		t.Errorf("want ErrWsTokenConflict, got %v", conflict)
	}
	se := errors.Serialize(conflict, errors.WithAttributes(errors.ServerOutputFormat))
	if se.StatusCode != 409 || se.Code != "VTL-0010" {
		t.Errorf("conflict = %d %s, want 409 VTL-0010", se.StatusCode, se.Code)
	}
	if tp, _ := wv.conns.payload(5); tp.(*ctxPayload).org == 1 {
		t.Error("the accepted payload was not stored")
	}
}

func TestWsCloseError(t *testing.T) {
	inner := ErrValidationFailed.New()
	err := fmt.Errorf("wrapped: %w", NewWsCloseError(WsStatusForbidden, "user changed", inner))
	var wce *WsCloseError
	if !stderrors.As(err, &wce) || wce.Code != WsStatusForbidden || wce.Reason != "user changed" {
		t.Fatalf("errors.As = %v", wce)
	}
	if !errors.Is(err, ErrValidationFailed) {
		t.Error("Unwrap must expose Err")
	}
	if got := NewWsCloseError(4009, "organization changed", nil).Error(); !strings.Contains(got, "4009") || !strings.Contains(got, "organization changed") {
		t.Errorf("Error() = %q, want the code and the reason", got)
	}
}

func TestTruncateCloseReason(t *testing.T) {
	a := func(n int) string { return strings.Repeat("a", n) }
	tests := []struct{ name, in, want string }{
		{"short", "bye", "bye"},
		{"123 bytes", a(123), a(123)},
		{"124 bytes", a(124), a(123)},
		{"2-byte rune across the limit", a(122) + "é" + "z", a(122)},
		{"3-byte rune across the limit", a(121) + "€" + "z", a(121)},
		{"rune ending at the limit", a(121) + "é" + "z", a(121) + "é"},
		{"invalid UTF-8", "a\xffb", "ab"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateCloseReason(tt.in)
			if got != tt.want || len(got) > 123 || !utf8.ValidString(got) {
				t.Errorf("truncateCloseReason = %q (%d bytes), want %q", got, len(got), tt.want)
			}
		})
	}
}

// WithWsAuthTimeout has no effect without a TokenDecoder: no token could
// authenticate a connection.
func TestWithWsAuthTimeout_NoDecoder(t *testing.T) {
	if d := NewWebsocketVatel(WithWsAuthTimeout(time.Second)).conns.authTimeout; d != 0 {
		t.Errorf("auth timeout without a decoder = %v, want 0", d)
	}
	if d := NewWebsocketVatel(WithTokenDecoder(ctxDecoder{}), WithWsAuthTimeout(time.Second)).conns.authTimeout; d != time.Second {
		t.Errorf("auth timeout = %v, want 1s", d)
	}
}

// remove stops the timer of the connection.
func TestWsConns_RemoveStopsTimer(t *testing.T) {
	cs := newWsConns()
	cs.authTimeout = time.Hour
	c := &websocket.Conn{}
	if err := cs.add(c, nil); err != nil {
		t.Fatal(err)
	}
	wc := cs.conns[c.ID()]
	if wc.timer == nil {
		t.Fatal("no auth timer armed by add")
	}
	cs.remove(c)
	if wc.timer != nil {
		t.Error("timer not stopped by remove")
	}
}

func TestUpdateAccessToken_DecodeError(t *testing.T) {
	wv := NewWebsocketVatel(WithTokenDecoder(&mockDecoder{err: stderrors.New("expired")}))
	err := wv.UpdateAccessToken(1, "bad")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("want ErrUnauthorized, got %v", err)
	}
	if _, ok := wv.conns.payload(1); ok {
		t.Error("payload must not be stored")
	}
}

// A decoder returning neither a token nor an error rejects the token.
func TestUpdateAccessToken_DecoderReturnsNilToken(t *testing.T) {
	wv := NewWebsocketVatel(WithTokenDecoder(&mockDecoder{}))
	err := wv.UpdateAccessToken(1, "token")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("want ErrUnauthorized, got %v", err)
	}
	if _, ok := wv.conns.payload(1); ok {
		t.Error("payload must not be stored")
	}
}

func TestUpdateAccessToken_NoDecoder(t *testing.T) {
	wv := NewWebsocketVatel()
	called := false
	wv.OnTokenUpdate(func(uint64, AuthPayload, AuthPayload) error { called = true; return nil })
	if err := wv.UpdateAccessToken(1, "token"); err != nil {
		t.Errorf("want nil without a decoder, got %v", err)
	}
	if called {
		t.Error("callback must not run without a decoder")
	}
	if _, ok := wv.conns.payload(1); ok {
		t.Error("nothing must be stored without a decoder")
	}
}

// ── wsConns (WS) ─────────────────────────────────────────────────────────────

func TestWsConns_SetGetRemove(t *testing.T) {
	cs := newWsConns()
	tp := &mockPayload{perms: []byte{1}}

	if _, ok := cs.payload(1); ok {
		t.Error("expected not found before set")
	}

	cs.setPayload(1, tp)
	got, ok := cs.payload(1)
	if !ok || got != AuthPayload(tp) {
		t.Fatalf("payload after set = %v, %v", got, ok)
	}

	// A payload stored before the connection opened becomes its payload.
	c := &websocket.Conn{} // ID 0
	cs.setPayload(c.ID(), tp)
	if err := cs.add(c, nil); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got, ok := cs.serving(c); !ok || got != AuthPayload(tp) {
		t.Fatalf("serving after add = %v, %v", got, ok)
	}
	if err := cs.add(&websocket.Conn{}, nil); err != errWsDuplicateID {
		t.Errorf("add of a duplicate ID = %v, want errWsDuplicateID", err)
	}
	if registered, _ := cs.remove(&websocket.Conn{}); registered {
		t.Error("remove of another connection with the same ID must not unregister it")
	}
	if registered, _ := cs.remove(c); !registered {
		t.Error("remove: want registered")
	}
	if _, ok := cs.payload(c.ID()); ok {
		t.Error("expected not found after remove")
	}
}

func TestWsConns_Overwrite(t *testing.T) {
	cs := newWsConns()
	first, second := &mockPayload{}, &mockPayload{}
	cs.setPayload(1, first)
	cs.setPayload(1, second)

	if got, ok := cs.payload(1); !ok || got != AuthPayload(second) {
		t.Errorf("expected the second payload after overwrite, got %v", got)
	}
}

func TestWsConns_Concurrent(t *testing.T) {
	cs := newWsConns()
	done := make(chan struct{})
	const n = 100

	go func() {
		for i := uint64(0); i < n; i++ {
			cs.setPayload(i, &mockPayload{})
		}
		close(done)
	}()
	for i := uint64(0); i < n; i++ {
		cs.payload(i)
	}
	<-done
}

// ── WS authFunc ──────────────────────────────────────────────────────────────

func TestWsCompile_PermsWithoutAuthFunc_Errors(t *testing.T) {
	wv := NewWebsocketVatel() // no authFunc
	e := &Endpoint{
		EndpointMeta:        EndpointMeta{Method: "WS", Path: "secret", Perms: []string{"admin"}},
		WebsocketController: func() WebsocketHandler { return &plainWsHandler{} },
	}
	v := NewVatel()
	l := discardLogger()
	if err := wv.compileWS(v, e, l); err == nil {
		t.Error("expected error when Perms declared without WithWsAuthFunc")
	}
}

func TestWsCompile_PermsWithAuthFunc_OK(t *testing.T) {
	authFn := func(ctx WebsocketContext, path string, requiredPerms []string) error { return nil }
	wv := NewWebsocketVatel(WithWsAuthFunc(authFn))
	e := &Endpoint{
		EndpointMeta:        EndpointMeta{Method: "WS", Path: "orders", Perms: []string{"orders:read"}},
		WebsocketController: func() WebsocketHandler { return &plainWsHandler{} },
	}
	v := NewVatel()
	l := discardLogger()
	if err := wv.compileWS(v, e, l); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// plainWsHandler is a minimal WebsocketHandler for compile tests.
type plainWsHandler struct{}

func (h *plainWsHandler) Handle(ctx WebsocketContext) error { return nil }

// A panic is a 500 and is alarmed, even when its value is a *WsCloseError.
func TestWsRecoverPanic_CloseErrorValueAlarmed(t *testing.T) {
	m := &syncMetrics{}
	e := &Endpoint{EndpointMeta: EndpointMeta{Method: "WS", Path: "x"}}
	compileCommon(NewVatel(WithAlarmer(m), WithMetricReporter(m)), e)
	var out bytes.Buffer
	e.wsRecoverPanic(&out, LogExit, false, zerolog.Nop().With(), time.Now(), NewWsCloseError(WsStatusForbidden, "x", nil))
	if !strings.Contains(out.String(), "VTL-0005") {
		t.Errorf("reply = %s, want VTL-0005", out.String())
	}
	if n := m.alarms(); n != 1 {
		t.Errorf("alarms = %d, want 1", n)
	}
}

// wsReplyError builds the reply from WsCloseError.Err only: an unrelated
// *errors.Error joined next to the close error does not set the reply.
func TestWsReplyError_FromCloseErrorOnly(t *testing.T) {
	v := NewVatel()
	tests := []struct {
		name     string
		err      error
		wantCode string
		wantSt   int
	}{
		{"unrelated error joined, Err nil", stderrors.Join(ErrValidationFailed.New(), NewWsCloseError(WsStatusForbidden, "x", nil)), "VTL-0003", 403},
		{"unrelated error joined after", stderrors.Join(NewWsCloseError(WsStatusContextChanged, "x", nil), ErrInvalidRequestBody.New()), "VTL-0009", 409},
		{"unrelated error joined, Err coded", stderrors.Join(ErrValidationFailed.New(), NewWsCloseError(WsStatusForbidden, "x", ErrUnknownPath.New())), "VTL-0008", 404},
		{"Err coded, wrapped", fmt.Errorf("ctx: %w", NewWsCloseError(WsStatusForbidden, "x", ErrValidationFailed.New())), "VTL-0006", 400},
		{"Err plain", NewWsCloseError(WsStatusAuthRequired, "x", stderrors.New("plain")), "VTL-0004", 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reply, wce := wsReplyError(tt.err)
			if wce == nil {
				t.Fatal("no WsCloseError found")
			}
			st, body, _ := v.errorResponse(reply, false)
			if st != tt.wantSt || !strings.Contains(string(body), `"code":"`+tt.wantCode+`"`) {
				t.Errorf("reply = %d %s, want %d %s", st, body, tt.wantSt, tt.wantCode)
			}
		})
	}
}

// UpdateAccessToken for a connection that is closing returns
// ErrWsTokenConflict and stores nothing, whether it was closing before the
// callback (the callback is not called) or started closing while it ran.
func TestUpdateAccessToken_ClosingConnection(t *testing.T) {
	wv := NewWebsocketVatel(WithTokenDecoder(ctxDecoder{}))
	c := &websocket.Conn{} // ID 0
	if err := wv.conns.add(c, nil); err != nil {
		t.Fatal(err)
	}
	wv.OnTokenUpdate(func(cid uint64, _, _ AuthPayload) error {
		if _, _, ok := wv.conns.startClose(cid, nil); !ok {
			t.Error("startClose in the callback = false")
		}
		return nil
	})
	if err := wv.UpdateAccessToken(c.ID(), "1/10"); !errors.Is(err, ErrWsTokenConflict) {
		t.Errorf("closing while the callback ran: err = %v, want ErrWsTokenConflict", err)
	}
	if _, ok := wv.conns.payload(c.ID()); ok {
		t.Error("payload stored for a closing connection")
	}

	calls := 0
	wv.OnTokenUpdate(func(uint64, AuthPayload, AuthPayload) error { calls++; return nil })
	if err := wv.UpdateAccessToken(c.ID(), "1/10"); !errors.Is(err, ErrWsTokenConflict) {
		t.Errorf("closing before: err = %v, want ErrWsTokenConflict", err)
	}
	if calls != 0 {
		t.Errorf("callback calls for a closing connection = %d, want 0", calls)
	}
	if _, ok := wv.conns.payload(c.ID()); ok {
		t.Error("payload stored for a closing connection")
	}
}

// A payload stored for an ID before its connection opened, which has expired
// by then, is dropped on open: the connection has no payload (the next token
// is the first, prev == nil) and the auth timeout applies.
func TestWsConns_AddDropsExpiredPayload(t *testing.T) {
	cs := newWsConns()
	cs.authTimeout = time.Hour
	c := &websocket.Conn{} // ID 0
	cs.storePayload(c.ID(), nil, 0, &mockPayload{}, time.Now().Add(-time.Second))
	if err := cs.add(c, nil); err != nil {
		t.Fatal(err)
	}
	if tp, ok := cs.serving(c); !ok || tp != nil {
		t.Errorf("serving = %v, %v; want no payload, served", tp, ok)
	}
	if _, prev, _, _ := cs.lookup(c.ID()); prev != nil {
		t.Errorf("prev = %v, want nil", prev)
	}
	wc := cs.conns[c.ID()]
	if wc.timer == nil || !wc.exp.IsZero() {
		t.Errorf("timer %v, exp %v; want the auth timer and no expiry", wc.timer, wc.exp)
	}
	cs.remove(c)

	// A payload that has not expired is kept.
	tp := &mockPayload{}
	cs.storePayload(c.ID(), nil, 0, tp, time.Now().Add(time.Hour))
	if err := cs.add(c, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := cs.serving(c); got != AuthPayload(tp) {
		t.Errorf("serving = %v, want the stored payload", got)
	}
	cs.remove(c)
}

// A payload that has expired does not authorize a message, even before the
// expiry timer closed the connection.
func TestWsConns_ServingExpiredPayload(t *testing.T) {
	cs := newWsConns()
	c := &websocket.Conn{}
	cs.conns[c.ID()] = &wsConn{conn: c, tp: &mockPayload{}, exp: time.Now().Add(-time.Millisecond), ver: 1, authed: true}
	if tp, ok := cs.serving(c); !ok || tp != nil {
		t.Errorf("serving = %v, %v; want no payload, served", tp, ok)
	}
}
