package vatel

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

// panicHandler writes a part of a response, then panics.
type panicHandler struct{}

func (panicHandler) Handle(ctx Context) error {
	ctx.SetStatusCode(fasthttp.StatusCreated)
	ctx.SetHeader([]byte("X-Partial"), []byte("1"))
	ctx.SetContentType([]byte("text/plain"))
	ctx.BodyWriter().Write([]byte("partial")) //nolint:errcheck
	panic("SECRET panic value")
}

// panicMarshal panics when it is marshalled.
type panicMarshal struct{}

func (panicMarshal) MarshalJSON() ([]byte, error) { panic("marshal boom") }

type panicResultHandler struct{}

func (panicResultHandler) Result() any              { return panicMarshal{} }
func (panicResultHandler) Handle(ctx Context) error { return nil }

// servePanic compiles e on v, serves one GET request and returns the request
// context and the log.
func servePanic(t *testing.T, v *Vatel, e Endpoint) (*fasthttp.RequestCtx, string) {
	t.Helper()
	var buf bytes.Buffer
	l := zerolog.New(&buf)
	if err := e.compile(v); err != nil {
		t.Fatalf("compile: %v", err)
	}
	fctx := newGETCtx("/p")
	e.handler(&l)(fctx)
	return fctx, buf.String()
}

// wantPanicResponse checks a recovered panic: a 500 VTL-0005 JSON error
// without the partial response, the panic logged with its stack, one metric
// and one alarm.
func wantPanicResponse(t *testing.T, fctx *fasthttp.RequestCtx, log string, m *mockMetrics, ala *mockAlarmer, panicValue string) {
	t.Helper()
	if got := fctx.Response.StatusCode(); got != 500 {
		t.Errorf("status = %d, want 500", got)
	}
	body := string(fctx.Response.Body())
	var eb struct {
		Msg  string `json:"msg"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &eb); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, body)
	}
	if eb.Code != "VTL-0005" || eb.Msg != "internal error" {
		t.Errorf("body = %s, want internal error VTL-0005", body)
	}
	if strings.Contains(body, panicValue) || strings.Contains(body, "partial") {
		t.Errorf("body leaks the panic or the partial response: %s", body)
	}
	if ct := string(fctx.Response.Header.ContentType()); ct != jsonContentType {
		t.Errorf("Content-Type = %q, want %q", ct, jsonContentType)
	}
	if h := fctx.Response.Header.Peek("X-Partial"); len(h) != 0 {
		t.Errorf("header of the partial response kept: X-Partial=%s", h)
	}
	if !strings.Contains(log, `"panic":"`+panicValue) {
		t.Errorf("panic value not logged: %s", log)
	}
	if !strings.Contains(log, `"stack":"goroutine`) {
		t.Errorf("stack not logged: %s", log)
	}
	if m.callCount != 1 || m.lastStatus != 500 {
		t.Errorf("metrics = %d calls, last status %d; want 1 call with 500", m.callCount, m.lastStatus)
	}
	if !ala.alarmed {
		t.Error("Alarmer not called")
	}
}

func TestRecover_HandlerPanic(t *testing.T) {
	m, ala := &mockMetrics{}, &mockAlarmer{}
	v := NewVatel(WithMetricReporter(m), WithAlarmer(ala))
	var errHook bool
	v.AddMiddleware(OnErrorResponse, func(Context, EndpointMeta) error { errHook = true; return nil })

	fctx, log := servePanic(t, v, GET("/p", func() Handler { return panicHandler{} }))

	wantPanicResponse(t, fctx, log, m, ala, "SECRET panic value")
	if !errHook {
		t.Error("OnErrorResponse not run for a recovered panic")
	}
}

func TestRecover_HandlerPanic_LogSilent(t *testing.T) {
	m, ala := &mockMetrics{}, &mockAlarmer{}
	v := NewVatel(WithMetricReporter(m), WithAlarmer(ala))

	fctx, log := servePanic(t, v, GET("/p", func() Handler { return panicHandler{} }, WithLogOption(LogSilent)))

	wantPanicResponse(t, fctx, log, m, ala, "SECRET panic value")
}

func TestRecover_HandlerPanic_Verbose(t *testing.T) {
	v := NewVatel(WithVerboseError(true))

	fctx, _ := servePanic(t, v, GET("/p", func() Handler { return panicHandler{} }))

	if got := fctx.Response.StatusCode(); got != 500 {
		t.Errorf("status = %d, want 500", got)
	}
	if body := string(fctx.Response.Body()); !strings.Contains(body, "SECRET panic value") || !strings.Contains(body, "VTL-0005") {
		t.Errorf("verbose body = %s, want VTL-0005 with the panic value", body)
	}
}

func TestRecover_MiddlewarePanic(t *testing.T) {
	for _, pos := range []MiddlewarePos{OnRequest, OnAuthentication, OnAuthorization, OnHandle} {
		m, ala := &mockMetrics{}, &mockAlarmer{}
		v := NewVatel(WithMetricReporter(m), WithAlarmer(ala))
		v.AddMiddleware(pos, func(Context, EndpointMeta) error { panic("mw boom") })
		h := &plainHandler{}

		fctx, log := servePanic(t, v, GET("/p", func() Handler { return h }))

		wantPanicResponse(t, fctx, log, m, ala, "mw boom")
		if h.called {
			t.Errorf("middleware %d: handler ran after a panic", pos)
		}
	}
}

func TestRecover_ResultMarshalPanic(t *testing.T) {
	m, ala := &mockMetrics{}, &mockAlarmer{}
	v := NewVatel(WithMetricReporter(m), WithAlarmer(ala))

	fctx, log := servePanic(t, v, GET("/p", func() Handler { return panicResultHandler{} }))

	wantPanicResponse(t, fctx, log, m, ala, "marshal boom")
}

// A panic in a hook after the response is written replaces the response with
// a 500 error; the request is reported to the metrics once.
func TestRecover_HookPanic(t *testing.T) {
	cases := []struct {
		name string
		pos  MiddlewarePos
		h    Handler
	}{
		{"OnSuccessResponse", OnSuccessResponse, &resultHandler{}},
		{"OnErrorResponse", OnErrorResponse, &plainHandler{retErr: ErrValidationFailed.New()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ala := &mockMetrics{}, &mockAlarmer{}
			v := NewVatel(WithMetricReporter(m), WithAlarmer(ala))
			var calls atomic.Int32
			v.AddMiddleware(tc.pos, func(Context, EndpointMeta) error { calls.Add(1); panic("hook boom") })

			fctx, log := servePanic(t, v, GET("/p", func() Handler { return tc.h }))

			if got := fctx.Response.StatusCode(); got != 500 {
				t.Errorf("status = %d, want 500", got)
			}
			if body := string(fctx.Response.Body()); !strings.Contains(body, "VTL-0005") || strings.Contains(body, "hello") {
				t.Errorf("body = %s, want only the VTL-0005 error", body)
			}
			if !strings.Contains(log, `"panic":"hook boom"`) {
				t.Errorf("panic not logged: %s", log)
			}
			if m.callCount != 1 {
				t.Errorf("metric calls = %d, want 1", m.callCount)
			}
			if !ala.alarmed {
				t.Error("Alarmer not called")
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("hook ran %d times, want 1", n)
			}
		})
	}
}

// A panicking OnErrorResponse hook does not escape the recovery of a handler
// panic.
func TestRecover_PanicInRecovery(t *testing.T) {
	v := NewVatel()
	v.AddMiddleware(OnErrorResponse, func(Context, EndpointMeta) error { panic("hook boom") })

	fctx, log := servePanic(t, v, GET("/p", func() Handler { return panicHandler{} }))

	if got := fctx.Response.StatusCode(); got != 500 {
		t.Errorf("status = %d, want 500", got)
	}
	if !strings.Contains(log, "hook boom") {
		t.Errorf("second panic not logged: %s", log)
	}
}

// The server keeps serving after a handler panic.
func TestRecover_ServerSurvives(t *testing.T) {
	ala := &mockAlarmer{}
	v := NewVatel(WithAlarmer(ala))
	v.Add(Endpoints{
		GET("/panic", func() Handler { return panicHandler{} }),
		GET("/ok", func() Handler { return &resultHandler{} }),
	})
	mux := router.New()
	if err := v.BuildHandlers(mux, discardLogger()); err != nil {
		t.Fatalf("BuildHandlers: %v", err)
	}
	ln := fasthttputil.NewInmemoryListener()
	served := make(chan error, 1)
	go func() { served <- v.serve(ln, mux) }()
	waitServing(t, v)
	defer func() {
		v.Shutdown(t.Context()) //nolint:errcheck
		<-served
	}()

	get := func(path string) string {
		t.Helper()
		conn, err := ln.Dial()
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		raw, _ := io.ReadAll(conn)
		return string(raw)
	}

	if got := get("/panic"); !strings.HasPrefix(got, "HTTP/1.1 500") || !strings.Contains(got, "VTL-0005") {
		t.Errorf("/panic response = %q, want 500 VTL-0005", got)
	}
	if got := get("/ok"); !strings.HasPrefix(got, "HTTP/1.1 200") || !strings.Contains(got, "hello") {
		t.Errorf("/ok after a panic = %q, want 200", got)
	}
	if !ala.alarmed {
		t.Error("Alarmer not called")
	}
}
