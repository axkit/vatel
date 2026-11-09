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
	"github.com/rs/zerolog"
)

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
