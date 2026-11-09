package vatel

import (
	"bytes"
	"context"
	stderrors "errors"
	"strings"
	"testing"
	"time"

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
	var gotTP AuthPayload
	var stored bool
	wv.OnTokenUpdate(func(cid uint64, tp AuthPayload) error {
		gotCID, gotTP = cid, tp
		_, stored = wv.conns.payload(cid)
		return nil
	})
	if err := wv.UpdateAccessToken(42, "token"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotCID != 42 || gotTP != AuthPayload(first) {
		t.Errorf("callback got cid=%d tp=%v", gotCID, gotTP)
	}
	if stored {
		t.Error("the callback must run before the payload is stored")
	}
	if tp, ok := wv.conns.payload(42); !ok || tp != AuthPayload(first) {
		t.Errorf("stored payload = %v, %v; want the accepted payload", tp, ok)
	}

	// A rejected token is not stored: the connection keeps its payload.
	errCb := stderrors.New("rejected")
	wv.OnTokenUpdate(func(uint64, AuthPayload) error { return errCb })
	dec.token = &mockTokener{payload: second}
	if err := wv.UpdateAccessToken(42, "token"); !stderrors.Is(err, errCb) {
		t.Errorf("want callback error, got %v", err)
	}
	if tp, _ := wv.conns.payload(42); tp != AuthPayload(first) {
		t.Errorf("stored payload = %v, want the previous payload after a rejection", tp)
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
	wv.OnTokenUpdate(func(uint64, AuthPayload) error { called = true; return nil })
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
