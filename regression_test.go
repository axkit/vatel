package vatel

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"testing"

	"github.com/axkit/date"
	"github.com/axkit/errors"
	"github.com/axkit/vatel/jsonmask"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

// ── request body log: non-canonical and duplicate keys ───────────────────────

func TestLog_RequestBody_NonCanonicalKeysMasked(t *testing.T) {
	bodies := map[string]string{
		"PascalCase key": `{"name":"a","Password":"SECRET1"}`,
		"duplicate key":  `{"name":"a","password":"x","password":"SECRET2"}`,
		"upper case key": `{"name":"a","PASSWORD":"SECRET3"}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			h := &secretBodyHandler{}
			fctx := newPOSTCtx("/items", []byte(body))
			runLogged(t, NewVatel(), POST("/items", func() Handler { return h },
				WithLogOption(LogFullOnExit)), fctx, &buf)

			if got := responseStatus(fctx); got != 200 {
				t.Fatalf("status = %d, want 200", got)
			}
			if !strings.HasPrefix(h.In.Password, "SECRET") {
				t.Fatalf("handler password = %q, want the secret", h.In.Password)
			}
			// The handler echoes the input; only the log must be masked.
			logged := strings.ReplaceAll(buf.String(), string(fctx.Response.Body()), "")
			if strings.Contains(logged, "SECRET") {
				t.Errorf("secret leaked to log: %s", buf.String())
			}
			m := lastLogLine(t, &buf)
			obj, ok := m["requestBody"].(map[string]interface{})
			if !ok || obj["password"] != jsonmask.DefaultMask || obj["name"] != "a" {
				t.Errorf("requestBody = %v, want password masked and name kept: %s", m["requestBody"], buf.String())
			}
		})
	}
}

func TestLog_RequestBody_InvalidJSON_WithMaskRules(t *testing.T) {
	var buf bytes.Buffer
	fctx := newPOSTCtx("/items", []byte(`{"password":"SECRET4",`))
	runLogged(t, NewVatel(), POST("/items", func() Handler { return &secretBodyHandler{} },
		WithLogOption(LogExit|LogReqBody)), fctx, &buf)

	if got := responseStatus(fctx); got != 400 {
		t.Fatalf("status = %d, want 400", got)
	}
	if strings.Contains(buf.String(), "SECRET4") {
		t.Errorf("secret leaked to log: %s", buf.String())
	}
	m := lastLogLine(t, &buf)
	if _, ok := m["requestBodyLoggingFailed"]; !ok {
		t.Errorf("requestBodyLoggingFailed missing: %s", buf.String())
	}
}

func TestLog_RequestBody_InvalidJSON_WithoutMaskRules(t *testing.T) {
	var buf bytes.Buffer
	fctx := newPOSTCtx("/items", []byte(`{"title":`))
	runLogged(t, NewVatel(), POST("/items", func() Handler { return &bodyHandler{} },
		WithLogOption(LogExit|LogReqBody)), fctx, &buf)

	m := lastLogLine(t, &buf)
	if _, ok := m["requestBodyLoggingFailed"]; !ok {
		t.Errorf("requestBodyLoggingFailed missing: %s", buf.String())
	}
}

// ── masked path/query params: parse errors ───────────────────────────────────

type secretPINPathHandler struct {
	PathParams[struct {
		PIN int `param:"pin" mask:"secret"`
	}]
}

func (h *secretPINPathHandler) Handle(Context) error { return nil }

type secretPINQueryHandler struct {
	QueryParams[struct {
		PIN  int    `param:"pin" mask:"secret"`
		Date string `param:"d"`
	}]
}

func (h *secretPINQueryHandler) Handle(Context) error { return nil }

func TestLog_MaskedParamParseError_NoLeak(t *testing.T) {
	cases := []struct {
		name, path, uri string
		ctrl            func() Handler
	}{
		{"path", "/p/{pin}", "/p/12SECRET34", func() Handler { return &secretPINPathHandler{} }},
		{"query", "/q", "/q?pin=12SECRET34", func() Handler { return &secretPINQueryHandler{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fctx, lines, raw := serveLogged(t, NewVatel(WithVerboseError(true)), Endpoints{
				GET(tc.path, tc.ctrl, WithLogOption(LogExit)),
			}, "GET", tc.uri)

			if got := responseStatus(fctx); got != 400 {
				t.Fatalf("status = %d, want 400", got)
			}
			if strings.Contains(raw, "SECRET") {
				t.Errorf("masked param leaked to log: %s", raw)
			}
			if strings.Contains(string(fctx.Response.Body()), "SECRET") {
				t.Errorf("masked param leaked to verbose response: %s", fctx.Response.Body())
			}
			if lines[0]["pin"] != jsonmask.DefaultMask {
				t.Errorf("pin = %v, want %q: %s", lines[0]["pin"], jsonmask.DefaultMask, raw)
			}
		})
	}
}

func TestLog_UnmaskedParamParseError_KeepsCause(t *testing.T) {
	fctx, _, _ := serveLogged(t, NewVatel(WithVerboseError(true)), Endpoints{
		GET("/p/{id}", func() Handler { return &pathHandler{} }, WithLogOption(LogExit)),
	}, "GET", "/p/abc")

	if got := responseStatus(fctx); got != 400 {
		t.Fatalf("status = %d, want 400", got)
	}
	if !strings.Contains(string(fctx.Response.Body()), "abc") {
		t.Errorf("verbose response should carry the parse error of an unmasked param: %s", fctx.Response.Body())
	}
}

// ── OnSuccessResponse error ──────────────────────────────────────────────────

func TestHandler_OnSuccessResponse_ErrorIgnored(t *testing.T) {
	v := NewVatel()
	var second, onError bool
	v.AddMiddleware(OnSuccessResponse,
		func(Context, EndpointMeta) error { return fmt.Errorf("boom") },
		func(Context, EndpointMeta) error { second = true; return nil },
	)
	v.AddMiddleware(OnErrorResponse, func(Context, EndpointMeta) error { onError = true; return nil })
	mr := &mockMetrics{}
	ala := &mockAlarmer{}
	v.cfg.mr, v.cfg.ala = mr, ala

	var buf bytes.Buffer
	fctx := newGETCtx("/items")
	runLogged(t, v, GET("/items", func() Handler { return &resultHandler{} }, WithLogOption(LogExit)), fctx, &buf)

	if got := responseStatus(fctx); got != 200 {
		t.Errorf("status = %d, want 200", got)
	}
	if got := string(fctx.Response.Body()); got != `{"message":"hello"}` {
		t.Errorf("body = %s, want the success body only", got)
	}
	if !second {
		t.Error("later OnSuccessResponse middleware was skipped")
	}
	if onError {
		t.Error("OnErrorResponse must not run")
	}
	if mr.callCount != 1 || mr.lastStatus != 200 {
		t.Errorf("metrics = %d calls, last status %d; want 1 call with 200", mr.callCount, mr.lastStatus)
	}
	if ala.alarmed {
		t.Error("Alarm must not fire")
	}
	if strings.Contains(buf.String(), "request failed") {
		t.Errorf("error logged: %s", buf.String())
	}
}

// ── error text disclosure ────────────────────────────────────────────────────

func TestHandler_PlainErrorText_HiddenFromClient(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"plain error", stderrors.New("pq: password authentication failed for user SECRET"), 500, "VTL-0005"},
		{"fmt wrapper of plain error", fmt.Errorf("db: %w", stderrors.New("SECRET dsn")), 500, "VTL-0005"},
		{"context canceled", fmt.Errorf("query SECRET: %w", context.Canceled), 503, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			fctx := newGETCtx("/items")
			runLogged(t, NewVatel(), GET("/items", func() Handler { return &plainHandler{retErr: tc.err} }), fctx, &buf)

			if got := responseStatus(fctx); got != tc.wantStatus {
				t.Errorf("status = %d, want %d", got, tc.wantStatus)
			}
			body := string(fctx.Response.Body())
			if strings.Contains(body, "SECRET") {
				t.Errorf("internal error text sent to client: %s", body)
			}
			if tc.wantCode != "" && !strings.Contains(body, tc.wantCode) {
				t.Errorf("body = %s, want code %s", body, tc.wantCode)
			}
			if !strings.Contains(buf.String(), "SECRET") {
				t.Errorf("the original error must stay in the log: %s", buf.String())
			}
		})
	}
}

func TestHandler_PlainErrorText_VerboseShown(t *testing.T) {
	fctx := newGETCtx("/items")
	run(NewVatel(WithVerboseError(true)), GET("/items", func() Handler {
		return &plainHandler{retErr: stderrors.New("detail for developers")}
	}), fctx)

	if !strings.Contains(string(fctx.Response.Body()), "detail for developers") {
		t.Errorf("verbose response should carry the error text: %s", fctx.Response.Body())
	}
}

func TestHandler_WrappedVatelError_KeepsStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"fmt wrapper of *errors.Error", fmt.Errorf("check: %w", ErrForbidden.New()), 403},
		{"template returned as is", ErrUnauthorized, 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fctx := newGETCtx("/items")
			run(NewVatel(), GET("/items", func() Handler { return &plainHandler{retErr: tc.err} }), fctx)

			if got := responseStatus(fctx); got != tc.want {
				t.Errorf("status = %d, want %d: %s", got, tc.want, fctx.Response.Body())
			}
			if strings.Contains(string(fctx.Response.Body()), "check:") {
				t.Errorf("wrapper text sent to client: %s", fctx.Response.Body())
			}
		})
	}
}

// ── BuildHandlers: router panics become errors ───────────────────────────────

func TestBuildHandlers_RouterConflicts_ReturnError(t *testing.T) {
	cases := []struct {
		name string
		eps  Endpoints
	}{
		{"path without leading slash", Endpoints{GET("orders", Func(func(Context) error { return nil }))}},
		{"duplicate route", Endpoints{
			GET("/a", Func(func(Context) error { return nil })),
			GET("/a", Func(func(Context) error { return nil })),
		}},
		{"own GET /", Endpoints{GET("/", Func(func(Context) error { return nil }))}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := NewVatel()
			v.Add(tc.eps)
			mux := router.New()
			l := zerolog.Nop()

			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("BuildHandlers panicked: %v", r)
					}
				}()
				err = v.BuildHandlers(mux, &l)
			}()
			if err == nil {
				t.Fatal("BuildHandlers: want an error")
			}
			if err2 := v.BuildHandlers(mux, &l); err2 == nil || !strings.Contains(err2.Error(), "previous build failed") {
				t.Errorf("second BuildHandlers = %v, want 'previous build failed'", err2)
			}
		})
	}
}

// ── masking error paths and custom masker ────────────────────────────────────

// failingMasker builds the default rules but fails to mask.
type failingMasker struct{ *jsonmask.RawJsonMask }

func (failingMasker) Mask([]byte, jsonmask.Fields) ([]byte, error) {
	return nil, stderrors.New("mask failed")
}

func TestLog_MaskingFailed_OnlyErrorLogged(t *testing.T) {
	var buf bytes.Buffer
	fctx := newPOSTCtx("/items", []byte(`{"name":"a","password":"p1"}`))
	runLogged(t, NewVatel(WithJsonMasker(failingMasker{jsonmask.New()})), POST("/items",
		func() Handler { return &secretBodyHandler{} }, WithLogOption(LogFullOnExit)), fctx, &buf)

	if got := responseStatus(fctx); got != 200 {
		t.Fatalf("status = %d, want 200", got)
	}
	if strings.Contains(buf.String(), "p1") {
		t.Errorf("unmasked secret logged after a masking error: %s", buf.String())
	}
	m := lastLogLine(t, &buf)
	for _, key := range []string{"requestBodyMaskingFailed", "reqInputMaskingFailed", "respBodyMaskingFailed"} {
		if m[key] != "mask failed" {
			t.Errorf("%s = %v, want the masking error: %s", key, m[key], buf.String())
		}
	}
	for _, key := range []string{"requestBody", "reqInput", "respBody"} {
		if _, ok := m[key]; ok {
			t.Errorf("%s must not be logged after a masking error", key)
		}
	}
}

func TestLog_WithJsonMasker_CustomFunc(t *testing.T) {
	jm := jsonmask.New()
	jm.AddFunc("secret", func(string) string { return "CUSTOM" })
	var buf bytes.Buffer
	fctx := newPOSTCtx("/items", []byte(`{"name":"a","password":"p1"}`))
	runLogged(t, NewVatel(WithJsonMasker(jm)), POST("/items",
		func() Handler { return &secretBodyHandler{} }, WithLogOption(LogFullOnExit)), fctx, &buf)

	m := lastLogLine(t, &buf)
	for _, key := range []string{"requestBody", "reqInput", "respBody"} {
		if obj, _ := m[key].(map[string]interface{}); obj["password"] != "CUSTOM" {
			t.Errorf("%s = %v, want the custom masker output", key, m[key])
		}
	}
}

// dynamicResultHandler returns an untyped nil from Result() until Handle sets
// out, so its result type is unknown at compile time.
type dynamicResultHandler struct{ out *secretItem }

func (h *dynamicResultHandler) Result() interface{} {
	if h.out == nil {
		return nil
	}
	return h.out
}

func (h *dynamicResultHandler) Handle(Context) error {
	h.out = &secretItem{Name: "a", Password: "p1"}
	return nil
}

func TestLog_DynamicResultMasked(t *testing.T) {
	v := NewVatel()
	e := GET("/items", func() Handler { return &dynamicResultHandler{} }, WithLogOption(LogExit|LogRespBody))
	if err := e.compile(v); err != nil {
		t.Fatal(err)
	}
	if e.resultFieldsByType == nil {
		t.Fatal("result rules must be built per type")
	}
	for i := 0; i < 2; i++ { // the second request uses the cached rules
		var buf bytes.Buffer
		l := zerolog.New(&buf)
		fctx := newGETCtx("/items")
		e.handler(&l)(fctx)
		if strings.Contains(buf.String(), "p1") {
			t.Errorf("secret leaked to log: %s", buf.String())
		}
		if obj, _ := lastLogLine(t, &buf)["respBody"].(map[string]interface{}); obj["password"] != jsonmask.DefaultMask {
			t.Errorf("respBody = %s, want password masked", buf.String())
		}
	}
}

// ── log level controls ───────────────────────────────────────────────────────

func TestLog_LevelControls(t *testing.T) {
	logLines := func(v *Vatel, e Endpoint, header string) string {
		if err := e.compile(v); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		l := zerolog.New(&buf)
		h := e.handler(&l)
		fctx := newGETCtx("/items")
		if header != "" {
			fctx.Request.Header.Set("X-Set-Log-Level", header)
		}
		h(fctx)
		return buf.String()
	}
	ok := func() Handler { return &plainHandler{} }

	t.Run("default logs nothing on success", func(t *testing.T) {
		if out := logLines(NewVatel(), GET("/items", ok), ""); out != "" {
			t.Errorf("logged: %s", out)
		}
	})
	t.Run("WithDefaultLogOption", func(t *testing.T) {
		if out := logLines(NewVatel(WithDefaultLogOption(LogExit)), GET("/items", ok), ""); !strings.Contains(out, "processed") {
			t.Errorf("want an exit line, got %q", out)
		}
	})
	t.Run("header affects one request only", func(t *testing.T) {
		v := NewVatel()
		e := GET("/items", ok)
		if err := e.compile(v); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		l := zerolog.New(&buf)
		h := e.handler(&l)
		fctx := newGETCtx("/items")
		fctx.Request.Header.Set("X-Set-Log-Level", "exit")
		h(fctx)
		if !strings.Contains(buf.String(), "processed") {
			t.Fatalf("header request not logged: %q", buf.String())
		}
		buf.Reset()
		h(newGETCtx("/items"))
		if buf.Len() != 0 {
			t.Errorf("request without header logged: %s", buf.String())
		}
	})
	t.Run("WithStaticLoggingLevel ignores header", func(t *testing.T) {
		if out := logLines(NewVatel(WithStaticLoggingLevel()), GET("/items", ok), "exit"); out != "" {
			t.Errorf("header honoured under WithStaticLoggingLevel: %s", out)
		}
	})
	t.Run("WithRequestID", func(t *testing.T) {
		out := logLines(NewVatel(WithRequestID()), GET("/items", ok, WithLogOption(LogEnter|LogExit)), "")
		if strings.Count(out, "\n") != 2 {
			t.Fatalf("want enter and exit lines: %s", out)
		}
		checkKeyCount(t, out, "reqId", 1)
		if out := logLines(NewVatel(), GET("/items", ok, WithLogOption(LogExit)), ""); strings.Contains(out, "reqId") {
			t.Errorf("reqId logged without WithRequestID: %s", out)
		}
	})
	t.Run("message user value", func(t *testing.T) {
		out := logLines(NewVatel(), GET("/items", Func(func(ctx Context) error {
			ctx.Set("message", 42) // not a string: must not panic
			return nil
		}), WithLogOption(LogExit)), "")
		if !strings.Contains(out, `"message":"42"`) || strings.Count(out, `"message"`) != 1 {
			t.Errorf("want message 42 once: %s", out)
		}
	})
}

// ── pipeline branches ────────────────────────────────────────────────────────

func TestHandler_QueryParams_InvalidValue(t *testing.T) {
	called := false
	fctx := newGETCtx("/q?page=abc")
	run(NewVatel(), GET("/q", func() Handler { return &countingQueryHandler{called: &called} }), fctx)
	if got := responseStatus(fctx); got != 400 || called {
		t.Errorf("status = %d, handler called = %v; want 400 and not called", got, called)
	}
}

func TestHandler_ErrorWithoutStatus_500(t *testing.T) {
	fctx := newGETCtx("/items")
	run(NewVatel(), GET("/items", func() Handler {
		return &plainHandler{retErr: errors.Wrap(fmt.Errorf("x"), "y")}
	}), fctx)
	if got := responseStatus(fctx); got != 500 {
		t.Errorf("status = %d, want 500", got)
	}
}

func TestHandler_OnHandleError(t *testing.T) {
	v := NewVatel()
	v.AddMiddleware(OnHandle, func(Context, EndpointMeta) error { return ErrForbidden.New() })
	h := &plainHandler{}
	fctx := newGETCtx("/items")
	run(v, GET("/items", func() Handler { return h }), fctx)
	if got := responseStatus(fctx); got != 403 || h.called {
		t.Errorf("status = %d, handler called = %v; want 403 and not called", got, h.called)
	}
}

func TestHandler_OnErrorResponse_MetaAtEveryPosition(t *testing.T) {
	fail := func(Context, EndpointMeta) error { return ErrForbidden.New() }
	cases := []struct {
		name  string
		setup func(v *Vatel) []EndpointOption
		ctrl  func() Handler
	}{
		{"OnRequest", func(v *Vatel) []EndpointOption { v.AddMiddleware(OnRequest, fail); return nil }, nil},
		{"OnAuthentication", func(v *Vatel) []EndpointOption { v.AddMiddleware(OnAuthentication, fail); return nil }, nil},
		{"OnHandle", func(v *Vatel) []EndpointOption { v.AddMiddleware(OnHandle, fail); return nil }, nil},
		{"handler", func(*Vatel) []EndpointOption { return nil }, func() Handler { return &plainHandler{retErr: ErrForbidden.New()} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := NewVatel()
			opts := tc.setup(v)
			var global, local EndpointMeta
			v.AddMiddleware(OnErrorResponse, func(_ Context, m EndpointMeta) error { global = m; return nil })
			opts = append(opts, func(e *Endpoint) {
				e.AddMiddleware(OnErrorResponse, func(_ Context, m EndpointMeta) error { local = m; return nil })
			})
			ctrl := tc.ctrl
			if ctrl == nil {
				ctrl = func() Handler { return &plainHandler{} }
			}
			fctx := newGETCtx("/items")
			run(v, GET("/items", ctrl, opts...), fctx)
			if got := responseStatus(fctx); got != 403 {
				t.Fatalf("status = %d, want 403", got)
			}
			for name, m := range map[string]EndpointMeta{"global": global, "per-endpoint": local} {
				if m.Method != "GET" || m.Path != "/items" {
					t.Errorf("%s OnErrorResponse meta = %+v, want GET /items", name, m)
				}
			}
		})
	}
}

func TestHandler_CompressRoute(t *testing.T) {
	v := NewVatel()
	v.Add(Endpoints{GET("/big", func() Handler { return &bigResultHandler{} }, WithCompress())})
	mux := router.New()
	l := zerolog.Nop()
	if err := v.BuildHandlers(mux, &l); err != nil {
		t.Fatal(err)
	}
	fctx := newGETCtx("/big")
	fctx.Request.Header.Set("Accept-Encoding", "gzip")
	mux.Handler(fctx)
	if got := string(fctx.Response.Header.Peek("Content-Encoding")); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
}

type bigResultHandler struct{ Output[string] }

func (h *bigResultHandler) Handle(Context) error {
	h.Out = strings.Repeat("compressible ", 200)
	return nil
}

func TestLog_SkippedOptionalBody_NotLogged(t *testing.T) {
	var buf bytes.Buffer
	fctx := newPOSTCtx("/items", nil)
	runLogged(t, NewVatel(), POST("/items", func() Handler { return &secretBodyHandler{} },
		WithOptionalBody(), WithLogOption(LogExit|LogReqBody|LogReqInput)), fctx, &buf)

	m := lastLogLine(t, &buf)
	for _, key := range []string{"requestBody", "reqInput", "requestBodyLoggingFailed"} {
		if _, ok := m[key]; ok {
			t.Errorf("%s logged for a skipped optional body: %s", key, buf.String())
		}
	}
}

func TestHandler_HEADMetricSizeZero(t *testing.T) {
	mr := &sizeMetrics{}
	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod("HEAD")
	fctx.Request.SetRequestURI("/items")
	run(NewVatel(WithMetricReporter(mr)), HEAD("/items", func() Handler { return &resultHandler{} }), fctx)
	if mr.size != 0 {
		t.Errorf("HEAD metric size = %d, want 0", mr.size)
	}
}

type sizeMetrics struct{ size int }

func (m *sizeMetrics) ReportMetric(_, _ string, _ int, _ float64, size int) { m.size = size }

type countingQueryHandler struct {
	QueryParams[struct {
		Page int `param:"page"`
	}]
	called *bool
}

func (c *countingQueryHandler) Handle(Context) error { *c.called = true; return nil }

// ── *date.Date path and query params ─────────────────────────────────────────

type ptrDateQueryHandler struct {
	QueryParams[struct {
		From *date.Date `param:"from"`
	}]
	got *date.Date
}

func (h *ptrDateQueryHandler) Handle(Context) error { h.got = h.Query.From; return nil }

type ptrDatePathHandler struct {
	PathParams[struct {
		Day *date.Date `param:"day"`
	}]
	got *date.Date
}

func (h *ptrDatePathHandler) Handle(Context) error { h.got = h.Params.Day; return nil }

func TestHandler_PtrDateParams(t *testing.T) {
	want, err := date.Parse("2024-01-02")
	if err != nil {
		t.Fatal(err)
	}

	qh := &ptrDateQueryHandler{}
	fctx, _, _ := serveLogged(t, NewVatel(), Endpoints{
		GET("/q", func() Handler { return qh }, WithLogOption(LogExit)),
	}, "GET", "/q?from=2024-01-02")
	if got := responseStatus(fctx); got != 200 {
		t.Fatalf("query: status = %d, want 200: %s", got, fctx.Response.Body())
	}
	if qh.got == nil || *qh.got != want {
		t.Errorf("query: From = %v, want %v", qh.got, want)
	}

	ph := &ptrDatePathHandler{}
	fctx, _, _ = serveLogged(t, NewVatel(), Endpoints{
		GET("/p/{day}", func() Handler { return ph }, WithLogOption(LogExit)),
	}, "GET", "/p/2024-01-02")
	if got := responseStatus(fctx); got != 200 {
		t.Fatalf("path: status = %d, want 200: %s", got, fctx.Response.Body())
	}
	if ph.got == nil || *ph.got != want {
		t.Errorf("path: Day = %v, want %v", ph.got, want)
	}
}

// ── Func on a parametrized path ──────────────────────────────────────────────

func TestFunc_PathParams(t *testing.T) {
	var id string
	fctx, _, _ := serveLogged(t, NewVatel(), Endpoints{
		DELETE("/orders/{id}", Func(func(ctx Context) error {
			id = ctx.PathValue("id")
			return nil
		}), WithLogOption(LogExit)),
	}, "DELETE", "/orders/42")
	if got := responseStatus(fctx); got != 200 {
		t.Fatalf("status = %d, want 200: %s", got, fctx.Response.Body())
	}
	if id != "42" {
		t.Errorf("PathValue(id) = %q, want 42", id)
	}
}

// ── BuildHandlers: unsupported param and query input types ──────────────────

type mapQueryHandler struct {
	QueryParams[map[string]string]
}

func (h *mapQueryHandler) Handle(Context) error { return nil }

type mapGetInputHandler struct {
	Body[map[string]string]
}

func (h *mapGetInputHandler) Handle(Context) error { return nil }

type intParamHandler struct{ id int }

func (h *intParamHandler) Param() interface{}   { return &h.id }
func (h *intParamHandler) Handle(Context) error { return nil }

type nilParamHandler struct{}

func (h *nilParamHandler) Param() interface{}   { return nil }
func (h *nilParamHandler) Handle(Context) error { return nil }

func TestBuildHandlers_NonStructParamInput_ReturnsError(t *testing.T) {
	cases := map[string]Endpoint{
		"QueryParams of a map": GET("/q", func() Handler { return &mapQueryHandler{} }),
		"GET Body of a map":    GET("/q", func() Handler { return &mapGetInputHandler{} }),
		"Param of an int":      GET("/p/{id}", func() Handler { return &intParamHandler{} }),
		"nil Param":            GET("/p/{id}", func() Handler { return &nilParamHandler{} }),
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			v := NewVatel()
			v.Add(Endpoints{e})
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("BuildHandlers panicked: %v", r)
					}
				}()
				err = v.BuildHandlers(router.New(), discardLogger())
			}()
			if err == nil {
				t.Fatal("BuildHandlers = nil, want an error")
			}
			if !strings.Contains(err.Error(), "struct") {
				t.Errorf("error = %v, want it to name the struct requirement", err)
			}
		})
	}
}

type cyclicFilter struct {
	Name   string        `param:"name"`
	Parent *cyclicFilter // ignored: a cycle
	Range  struct {
		Min int `param:"min"`
	}
}

type cyclicQueryHandler struct {
	QueryParams[cyclicFilter]
}

func (h *cyclicQueryHandler) Handle(Context) error { return nil }

func TestBuildHandlers_CyclicQueryType(t *testing.T) {
	h := &cyclicQueryHandler{}
	fctx, _, _ := serveLogged(t, NewVatel(), Endpoints{
		GET("/q", func() Handler { return h }, WithLogOption(LogExit)),
	}, "GET", "/q?name=a&min=3")
	if got := responseStatus(fctx); got != 200 {
		t.Fatalf("status = %d, want 200: %s", got, fctx.Response.Body())
	}
	if h.Query.Name != "a" || h.Query.Range.Min != 3 || h.Query.Parent != nil {
		t.Errorf("query = %+v, want name a, min 3 and no parent", h.Query)
	}
}

// ── masked body fields: JSON decode errors ───────────────────────────────────

type maskedPINBodyHandler struct {
	Body[struct {
		Name string `json:"name"`
		PIN  int64  `json:"pin" mask:"-"`
	}]
}

func (h *maskedPINBodyHandler) Handle(Context) error { return nil }

type plainPINBodyHandler struct {
	Body[struct {
		PIN int64 `json:"pin"`
	}]
}

func (h *plainPINBodyHandler) Handle(Context) error { return nil }

func TestLog_MaskedBodyDecodeError_NoLeak(t *testing.T) {
	cases := map[string]struct{ body, secret string }{
		"overflow": {`{"pin":99999999999999999999123}`, "99999999999999999999123"},
		"float":    {`{"pin":1234.5678}`, "1234.5678"},
		"string":   {`{"pin":"98765SECRET"}`, "98765SECRET"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			fctx := newPOSTCtx("/pin", []byte(tc.body))
			runLogged(t, NewVatel(WithVerboseError(true)), POST("/pin",
				func() Handler { return &maskedPINBodyHandler{} },
				WithLogOption(LogFullOnExit)), fctx, &buf)

			if got := responseStatus(fctx); got != 400 {
				t.Fatalf("status = %d, want 400", got)
			}
			if strings.Contains(buf.String(), tc.secret) {
				t.Errorf("masked body value leaked to log: %s", buf.String())
			}
			if strings.Contains(string(fctx.Response.Body()), tc.secret) {
				t.Errorf("masked body value leaked to verbose response: %s", fctx.Response.Body())
			}
			if !strings.Contains(buf.String(), `"field":"pin"`) {
				t.Errorf("log must name the field of the type mismatch: %s", buf.String())
			}
			if !strings.Contains(buf.String(), "VTL-0007") {
				t.Errorf("log must carry ErrInvalidRequestBody: %s", buf.String())
			}
		})
	}
}

func TestLog_UnmaskedBodyDecodeError_KeepsCause(t *testing.T) {
	var buf bytes.Buffer
	fctx := newPOSTCtx("/pin", []byte(`{"pin":1234.5678}`))
	runLogged(t, NewVatel(WithVerboseError(true)), POST("/pin",
		func() Handler { return &plainPINBodyHandler{} },
		WithLogOption(LogExit)), fctx, &buf)

	if got := responseStatus(fctx); got != 400 {
		t.Fatalf("status = %d, want 400", got)
	}
	if !strings.Contains(buf.String(), "1234.5678") {
		t.Errorf("the decode error of a body without mask rules should keep its cause: %s", buf.String())
	}
}
