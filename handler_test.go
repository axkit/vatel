package vatel

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/axkit/errors"
	"github.com/axkit/vatel/jsonmask"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// run compiles e and invokes the handler against fctx.
func run(v *Vatel, e Endpoint, fctx *fasthttp.RequestCtx) {
	if err := e.compile(v); err != nil {
		panic("compile: " + err.Error())
	}
	e.handler(discardLogger())(fctx)
}

// ── basic handler lifecycle ───────────────────────────────────────────────────

func TestHandler_PlainSuccess(t *testing.T) {
	h := &plainHandler{}
	v := noAuthVatel()
	fctx := newGETCtx("/items")

	run(v, GET("/items", func() Handler { return h }), fctx)

	if !h.called {
		t.Error("handler was not called")
	}
	if responseStatus(fctx) != 200 {
		t.Errorf("want 200, got %d", responseStatus(fctx))
	}
}

func TestHandler_HandlerError(t *testing.T) {
	h := &plainHandler{retErr: fmt.Errorf("something went wrong")}
	v := noAuthVatel()
	fctx := newGETCtx("/items")

	run(v, GET("/items", func() Handler { return h }), fctx)

	if responseStatus(fctx) != 500 {
		t.Errorf("want 500, got %d", responseStatus(fctx))
	}
	if len(fctx.Response.Body()) == 0 {
		t.Error("expected non-empty error response body")
	}
}

func TestHandler_StructuredError(t *testing.T) {
	sentinel := ErrValidationFailed.New().Msg("bad input")
	h := &plainHandler{retErr: sentinel}
	v := noAuthVatel()
	fctx := newGETCtx("/items")

	run(v, GET("/items", func() Handler { return h }), fctx)

	if responseStatus(fctx) != 400 {
		t.Errorf("want 400, got %d", responseStatus(fctx))
	}
	body := fctx.Response.Body()
	if len(body) == 0 {
		t.Error("expected non-empty error body")
	}
}

// ── query param decoding ──────────────────────────────────────────────────────

func TestHandler_QueryParams(t *testing.T) {
	h := &queryHandler{}
	v := noAuthVatel()
	fctx := newGETCtx("/items?id=7&name=foo")

	run(v, GET("/items", func() Handler { return h }), fctx)

	if !h.called {
		t.Fatal("handler not called")
	}
	if h.in.ID != 7 {
		t.Errorf("ID: want 7, got %d", h.in.ID)
	}
	if h.in.Name != "foo" {
		t.Errorf("Name: want foo, got %s", h.in.Name)
	}
}

func TestHandler_QueryParams_InvalidType(t *testing.T) {
	h := &queryHandler{}
	v := noAuthVatel()
	fctx := newGETCtx("/items?id=notanumber")

	run(v, GET("/items", func() Handler { return h }), fctx)

	if responseStatus(fctx) != 400 {
		t.Errorf("want 400 for invalid query param, got %d", responseStatus(fctx))
	}
	if h.called {
		t.Error("handler should not have been called")
	}
}

// ── path param decoding ───────────────────────────────────────────────────────

func TestHandler_PathParams(t *testing.T) {
	h := &pathHandler{}
	v := noAuthVatel()
	fctx := newGETCtx("/items/99")
	fctx.SetUserValue("id", "99")

	run(v, GET("/items/{id}", func() Handler { return h }), fctx)

	if !h.called {
		t.Fatal("handler not called")
	}
	if h.param.ID != 99 {
		t.Errorf("param.ID: want 99, got %d", h.param.ID)
	}
}

func TestHandler_PathParams_Missing(t *testing.T) {
	h := &pathHandler{}
	v := noAuthVatel()
	fctx := newGETCtx("/items/")
	// intentionally not setting user value "id"

	run(v, GET("/items/{id}", func() Handler { return h }), fctx)

	if responseStatus(fctx) != 500 {
		t.Errorf("want 500 for missing path param, got %d", responseStatus(fctx))
	}
	if h.called {
		t.Error("handler should not be called")
	}
}

// ── POST body decoding ────────────────────────────────────────────────────────

func TestHandler_PostBody(t *testing.T) {
	h := &bodyHandler{}
	v := noAuthVatel()
	fctx := newPOSTCtx("/items", []byte(`{"title":"test","count":3}`))

	run(v, POST("/items", func() Handler { return h }), fctx)

	if !h.called {
		t.Fatal("handler not called")
	}
	if h.in.Title != "test" || h.in.Count != 3 {
		t.Errorf("unexpected input: %+v", h.in)
	}
}

func TestHandler_PostBody_Empty(t *testing.T) {
	h := &bodyHandler{}
	v := noAuthVatel()
	fctx := newPOSTCtx("/items", nil)

	run(v, POST("/items", func() Handler { return h }), fctx)

	if responseStatus(fctx) != 400 {
		t.Errorf("want 400 for empty body, got %d", responseStatus(fctx))
	}
	if h.called {
		t.Error("handler should not be called with empty body")
	}
}

func TestHandler_PostBody_InvalidJSON(t *testing.T) {
	h := &bodyHandler{}
	v := noAuthVatel()
	fctx := newPOSTCtx("/items", []byte(`not json`))

	run(v, POST("/items", func() Handler { return h }), fctx)

	if responseStatus(fctx) != 400 {
		t.Errorf("want 400 for invalid JSON, got %d", responseStatus(fctx))
	}
}

// ── response body (Resulter) ──────────────────────────────────────────────────

func TestHandler_ResponseBody(t *testing.T) {
	h := &resultHandler{}
	v := noAuthVatel()
	fctx := newGETCtx("/items")

	run(v, GET("/items", func() Handler { return h }), fctx)

	body := fctx.Response.Body()
	if len(body) == 0 {
		t.Fatal("expected non-empty response body")
	}

	var res struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if res.Message != "hello" {
		t.Errorf("want hello, got %s", res.Message)
	}

	ct := string(fctx.Response.Header.Peek("Content-Type"))
	if ct != "application/json; charset=utf-8" {
		t.Errorf("unexpected Content-Type: %s", ct)
	}
}

func TestHandler_NilResult_NoBody(t *testing.T) {
	h := &nilResultHandler{} // out is nil — Result() returns nil
	v := noAuthVatel()
	fctx := newGETCtx("/items")

	run(v, GET("/items", func() Handler { return h }), fctx)

	if responseStatus(fctx) != 200 {
		t.Errorf("want 200, got %d", responseStatus(fctx))
	}
	if len(fctx.Response.Body()) != 0 {
		t.Errorf("want empty body for nil result, got: %s", fctx.Response.Body())
	}
}

// ── authorization ─────────────────────────────────────────────────────────────

func TestHandler_Auth_MissingHeader(t *testing.T) {
	v, _, _, cfg := authVatel(true)
	h := &plainHandler{}
	fctx := newGETCtx("/secret")

	run(v, GET("/secret", func() Handler { return h }, cfg.RequirePerms(map[string]uint{"read": 0})), fctx)

	if responseStatus(fctx) != 401 {
		t.Errorf("want 401, got %d", responseStatus(fctx))
	}
	if h.called {
		t.Error("handler should not be called without token")
	}
}

func TestHandler_Auth_ValidToken(t *testing.T) {
	v, _, _, cfg := authVatel(true)
	h := &tokenHandler{}
	fctx := newGETCtx("/secret")
	fctx.Request.Header.Set("Authorization", "Bearer valid-token")

	run(v, GET("/secret", func() Handler { return h }, cfg.RequirePerms(map[string]uint{"read": 0})), fctx)

	if responseStatus(fctx) != 200 {
		t.Errorf("want 200, got %d", responseStatus(fctx))
	}
	if h.payload == nil {
		t.Error("token payload should be set in context")
	}
	if h.payload.(*mockPayload).Login() != "alice" {
		t.Errorf("want login=alice, got %s", h.payload.(*mockPayload).Login())
	}
}

func TestHandler_Auth_Forbidden(t *testing.T) {
	v, _, _, cfg := authVatel(false) // allowed=false
	h := &plainHandler{}
	fctx := newGETCtx("/secret")
	fctx.Request.Header.Set("Authorization", "Bearer token")

	run(v, GET("/secret", func() Handler { return h }, cfg.RequirePerms(map[string]uint{"write": 0})), fctx)

	if responseStatus(fctx) != 403 {
		t.Errorf("want 403, got %d", responseStatus(fctx))
	}
	if h.called {
		t.Error("handler should not be called when forbidden")
	}
}

func TestHandler_Auth_RevokedToken(t *testing.T) {
	payload := &mockPayload{login: "alice", role: 1, perms: []byte{0xFF}}
	token := &mockTokener{payload: payload}
	dec := &mockDecoder{token: token}
	auth := &mockAuthorizer{allowed: true}
	revoker := &mockRevoker{revoked: true}

	v := NewVatel()
	v.AddMiddleware(OnAuthentication, func(ctx Context, _ EndpointMeta) error {
		at := ctx.Header("Authorization")
		if len(at) == 0 {
			return nil
		}
		revoked, err := revoker.IsTokenRevoked(string(at))
		if err != nil {
			return err
		}
		if revoked {
			return ErrAccessTokenRevoked.New()
		}
		tok, err2 := dec.Decode(at)
		if err2 != nil {
			return ErrUnauthorized.Wrap(err2)
		}
		ctx.SetTokenPayload(tok.ApplicationPayload())
		return nil
	})
	cfg := authConfig(dec, auth)

	h := &plainHandler{}
	fctx := newGETCtx("/secret")
	fctx.Request.Header.Set("Authorization", "Bearer revoked-token")

	run(v, GET("/secret", func() Handler { return h }, cfg.RequirePerms(map[string]uint{"read": 0})), fctx)

	if responseStatus(fctx) != 401 {
		t.Errorf("want 401 for revoked token, got %d", responseStatus(fctx))
	}
	if h.called {
		t.Error("handler should not be called with revoked token")
	}
}

func TestHandler_Auth_PublicEndpoint_TokenOptional(t *testing.T) {
	// Endpoint has no Perms — token header should be optional/ignored.
	v, _, _, _ := authVatel(true)
	h := &plainHandler{}
	fctx := newGETCtx("/public")
	// No Authorization header.

	run(v, GET("/public", func() Handler { return h }), fctx)

	if !h.called {
		t.Error("public handler should be called without token")
	}
	if responseStatus(fctx) != 200 {
		t.Errorf("want 200, got %d", responseStatus(fctx))
	}
}

// ── middleware ────────────────────────────────────────────────────────────────

func TestHandler_OnAuthentication_Success(t *testing.T) {
	v := noAuthVatel()
	var mwCalled bool
	v.AddMiddleware(OnAuthentication, func(ctx Context, _ EndpointMeta) error {
		mwCalled = true
		ctx.Set("key", "mw-value")
		return nil
	})

	h := &plainHandler{}
	fctx := newGETCtx("/items")
	run(v, GET("/items", func() Handler { return h }), fctx)

	if !mwCalled {
		t.Error("OnAuthentication middleware not called")
	}
	if !h.called {
		t.Error("handler not called")
	}
	if h.ctxVal != "mw-value" {
		t.Errorf("context value not propagated: got %v", h.ctxVal)
	}
}

func TestHandler_OnAuthentication_StopsPipeline(t *testing.T) {
	v := noAuthVatel()
	v.AddMiddleware(OnAuthentication, func(ctx Context, _ EndpointMeta) error {
		return ErrForbidden.New()
	})

	h := &plainHandler{}
	fctx := newGETCtx("/items")
	run(v, GET("/items", func() Handler { return h }), fctx)

	if responseStatus(fctx) != 403 {
		t.Errorf("want 403, got %d", responseStatus(fctx))
	}
	if h.called {
		t.Error("handler must not run when middleware returns error")
	}
}

func TestHandler_OnAuthorization_RunsBeforeHandler(t *testing.T) {
	v := noAuthVatel()
	var order []string

	v.AddMiddleware(OnAuthorization, func(ctx Context, _ EndpointMeta) error {
		order = append(order, "after-auth")
		return nil
	})

	h := &plainHandler{}
	origHandle := h.Handle
	_ = origHandle // just capturing call order via h.called is enough

	// Wrap handler to record order.
	called := false
	fctx := newGETCtx("/items")
	run(v, GET("/items", func() Handler {
		return &orderTracker{order: &order, label: "handler", inner: h}
	}), fctx)

	if len(order) < 2 {
		t.Fatalf("want at least 2 events, got %v", order)
	}
	if order[0] != "after-auth" || order[1] != "handler" {
		t.Errorf("wrong order: %v", order)
	}
	_ = called
}

func TestHandler_OnAuthorization_StopsPipeline(t *testing.T) {
	v := noAuthVatel()
	v.AddMiddleware(OnAuthorization, func(ctx Context, _ EndpointMeta) error {
		return ErrForbidden.New()
	})

	h := &plainHandler{}
	fctx := newGETCtx("/items")
	run(v, GET("/items", func() Handler { return h }), fctx)

	if responseStatus(fctx) != 403 {
		t.Errorf("want 403, got %d", responseStatus(fctx))
	}
	if h.called {
		t.Error("handler must not run when OnAuthorization middleware errors")
	}
}

func TestHandler_OnSuccessResponse_Called(t *testing.T) {
	v := noAuthVatel()
	var successCalled bool
	v.AddMiddleware(OnSuccessResponse, func(ctx Context, _ EndpointMeta) error {
		successCalled = true
		return nil
	})

	fctx := newGETCtx("/items")
	run(v, GET("/items", func() Handler { return &plainHandler{} }), fctx)

	if !successCalled {
		t.Error("OnSuccessResponse middleware not called on success")
	}
}

func TestHandler_OnSuccessResponse_NotCalledOnError(t *testing.T) {
	v := noAuthVatel()
	var successCalled bool
	v.AddMiddleware(OnSuccessResponse, func(ctx Context, _ EndpointMeta) error {
		successCalled = true
		return nil
	})

	fctx := newGETCtx("/items")
	run(v, GET("/items", func() Handler {
		return &plainHandler{retErr: fmt.Errorf("boom")}
	}), fctx)

	if successCalled {
		t.Error("OnSuccessResponse must not be called when handler errors")
	}
}

func TestHandler_OnErrorResponse_CalledOnError(t *testing.T) {
	v := noAuthVatel()
	var errCalled bool
	v.AddMiddleware(OnErrorResponse, func(ctx Context, _ EndpointMeta) error {
		errCalled = true
		return nil
	})

	fctx := newGETCtx("/items")
	run(v, GET("/items", func() Handler {
		return &plainHandler{retErr: fmt.Errorf("boom")}
	}), fctx)

	if !errCalled {
		t.Error("OnErrorResponse middleware not called on handler error")
	}
}

func TestHandler_OnErrorResponse_NotCalledOnSuccess(t *testing.T) {
	v := noAuthVatel()
	var errCalled bool
	v.AddMiddleware(OnErrorResponse, func(ctx Context, _ EndpointMeta) error {
		errCalled = true
		return nil
	})

	fctx := newGETCtx("/items")
	run(v, GET("/items", func() Handler { return &plainHandler{} }), fctx)

	if errCalled {
		t.Error("OnErrorResponse must not be called on success")
	}
}

// ── metrics ───────────────────────────────────────────────────────────────────

func TestHandler_Metrics_Success(t *testing.T) {
	m := &mockMetrics{}
	v := noAuthVatel(WithMetricReporter(m))
	fctx := newGETCtx("/items")

	run(v, GET("/items", func() Handler { return &plainHandler{} }), fctx)

	if m.callCount != 1 {
		t.Fatalf("expected 1 metric call, got %d", m.callCount)
	}
	if m.lastStatus != 200 {
		t.Errorf("want status 200 in metric, got %d", m.lastStatus)
	}
}

func TestHandler_Metrics_Error(t *testing.T) {
	m := &mockMetrics{}
	v := noAuthVatel(WithMetricReporter(m))
	fctx := newGETCtx("/items")

	run(v, GET("/items", func() Handler {
		return &plainHandler{retErr: ErrValidationFailed.New()}
	}), fctx)

	if m.callCount != 1 {
		t.Fatalf("expected 1 metric call, got %d", m.callCount)
	}
	if m.lastStatus != 400 {
		t.Errorf("want status 400 in metric, got %d", m.lastStatus)
	}
}

// ── alarmer ───────────────────────────────────────────────────────────────────

func TestHandler_Alarmer_TriggeredOn5xx(t *testing.T) {
	ala := &mockAlarmer{}
	v := noAuthVatel(WithAlarmer(ala))
	fctx := newGETCtx("/items")

	run(v, GET("/items", func() Handler {
		return &plainHandler{retErr: ErrInternalError.New()}
	}), fctx)

	if !ala.alarmed {
		t.Error("alarmer should be triggered for 5xx errors")
	}
}

func TestHandler_Alarmer_NotTriggeredOn4xx(t *testing.T) {
	ala := &mockAlarmer{}
	v := noAuthVatel(WithAlarmer(ala))
	fctx := newGETCtx("/items")

	run(v, GET("/items", func() Handler {
		return &plainHandler{retErr: ErrValidationFailed.New()}
	}), fctx)

	if ala.alarmed {
		t.Error("alarmer must not be triggered for 4xx errors")
	}
}

// ── endpoint options ──────────────────────────────────────────────────────────

func TestHandler_LogSilent_NoResponse(t *testing.T) {
	// With LogSilent the handler still processes normally; just no logging.
	h := &plainHandler{}
	v := noAuthVatel()
	fctx := newGETCtx("/items")

	run(v, GET("/items", func() Handler { return h }, WithLogOption(LogSilent)), fctx)

	if !h.called {
		t.Error("handler should be called even with LogSilent")
	}
}

func TestHandler_CustomSuccessStatusCode(t *testing.T) {
	m := &mockMetrics{}
	v := noAuthVatel(WithMetricReporter(m))
	fctx := newPOSTCtx("/items", []byte(`{"title":"x","count":1}`))

	run(v, POST("/items", func() Handler { return &bodyHandler{} },
		WithSuccessStatusCode(201)), fctx)

	if responseStatus(fctx) != 201 {
		t.Errorf("want 201, got %d", responseStatus(fctx))
	}
	if m.lastStatus != 201 {
		t.Errorf("want status 201 in metric, got %d", m.lastStatus)
	}
}

func TestHandler_SuccessStatusCode_NotAppliedOnError(t *testing.T) {
	v := noAuthVatel()
	fctx := newGETCtx("/items")

	run(v, GET("/items", func() Handler {
		return &plainHandler{retErr: ErrValidationFailed.New()}
	}, WithSuccessStatusCode(201)), fctx)

	if responseStatus(fctx) != 400 {
		t.Errorf("want 400, got %d", responseStatus(fctx))
	}
}

// statusHandler sets the response status itself.
type statusHandler struct{ code int }

func (h *statusHandler) Handle(ctx Context) error {
	ctx.SetStatusCode(h.code)
	return nil
}

func TestHandler_ManualStatusCode(t *testing.T) {
	v := noAuthVatel()
	fctx := newGETCtx("/items")

	run(v, GET("/items", func() Handler { return &statusHandler{code: 202} },
		WithSuccessStatusCode(201), WithManualStatusCode()), fctx)

	if responseStatus(fctx) != 202 {
		t.Errorf("want 202 set by handler, got %d", responseStatus(fctx))
	}
}

// ── client error fields ───────────────────────────────────────────────────────

func TestHandler_ErrorFields_HiddenFromClient(t *testing.T) {
	v := noAuthVatel()
	fctx := newGETCtx("/items")

	run(v, GET("/items", func() Handler {
		return &plainHandler{retErr: ErrValidationFailed.New().Set("reason", "name taken").Set("dbHost", "10.0.0.5")}
	}), fctx)

	var got map[string]any
	if err := json.Unmarshal(fctx.Response.Body(), &got); err != nil {
		t.Fatalf("invalid error JSON %q: %v", fctx.Response.Body(), err)
	}
	if _, ok := got["fields"]; ok {
		t.Errorf("fields must not reach the client: %s", fctx.Response.Body())
	}
	if _, ok := got["reason"]; ok {
		t.Errorf("reason must not reach the client without WithClientErrorFields: %s", fctx.Response.Body())
	}
}

func TestHandler_WithClientErrorFields(t *testing.T) {
	v := noAuthVatel(WithClientErrorFields("reason", "suggestedName"))
	fctx := newGETCtx("/items")

	run(v, GET("/items", func() Handler {
		return &plainHandler{retErr: ErrValidationFailed.New().
			Set("reason", `name "x" taken`).
			Set("suggestedName", "x2").
			Set("dbHost", "10.0.0.5")}
	}), fctx)

	var got map[string]any
	if err := json.Unmarshal(fctx.Response.Body(), &got); err != nil {
		t.Fatalf("invalid error JSON %q: %v", fctx.Response.Body(), err)
	}
	if got["reason"] != `name "x" taken` {
		t.Errorf("want reason at root, got %s", fctx.Response.Body())
	}
	if got["suggestedName"] != "x2" {
		t.Errorf("want suggestedName at root, got %s", fctx.Response.Body())
	}
	if _, ok := got["fields"]; ok {
		t.Errorf("other fields must not reach the client: %s", fctx.Response.Body())
	}
}

// ── build ─────────────────────────────────────────────────────────────────────

func TestBuildHandlers_Twice(t *testing.T) {
	v := noAuthVatel()
	v.Add(Endpoints{GET("/items", func() Handler { return &plainHandler{} })})
	mux := router.New()

	if err := v.BuildHandlers(mux, discardLogger()); err != nil {
		t.Fatalf("first build: %v", err)
	}
	if err := v.BuildHandlers(mux, discardLogger()); err == nil {
		t.Error("second build must return an error, not register routes again")
	}
}

// ── context ───────────────────────────────────────────────────────────────────

func TestContext_SetGet(t *testing.T) {
	fctx := &fasthttp.RequestCtx{}
	ctx := NewContext(fctx)

	ctx.Set("foo", 42)
	if v := ctx.Get("foo"); v != 42 {
		t.Errorf("want 42, got %v", v)
	}
}

func TestContext_PathValue(t *testing.T) {
	fctx := &fasthttp.RequestCtx{}
	fctx.SetUserValue("id", "99")
	ctx := NewContext(fctx)

	if v := ctx.PathValue("id"); v != "99" {
		t.Errorf("want 99, got %s", v)
	}
	if v := ctx.PathValue("missing"); v != "" {
		t.Errorf("want empty string for missing value, got %s", v)
	}
}

func TestContext_QueryValue(t *testing.T) {
	fctx := &fasthttp.RequestCtx{}
	fctx.QueryArgs().Add("page", "2")
	ctx := NewContext(fctx)

	if v := ctx.QueryValue("page"); string(v) != "2" {
		t.Errorf("want 2, got %s", v)
	}
}

func TestContext_SetStatusCode(t *testing.T) {
	fctx := &fasthttp.RequestCtx{}
	ctx := NewContext(fctx)
	ctx.SetStatusCode(404)
	if fctx.Response.StatusCode() != 404 {
		t.Errorf("want 404, got %d", fctx.Response.StatusCode())
	}
}

func TestContext_Header(t *testing.T) {
	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.Set("X-Custom", "hello")
	ctx := NewContext(fctx)
	if string(ctx.Header("X-Custom")) != "hello" {
		t.Errorf("want hello, got %s", ctx.Header("X-Custom"))
	}
}

func TestContext_SetHeader(t *testing.T) {
	fctx := &fasthttp.RequestCtx{}
	ctx := NewContext(fctx)
	ctx.SetHeader([]byte("X-Out"), []byte("world"))
	if string(fctx.Response.Header.Peek("X-Out")) != "world" {
		t.Error("response header not set")
	}
}

func TestContext_TokenPayload(t *testing.T) {
	fctx := &fasthttp.RequestCtx{}
	ctx := NewContext(fctx)

	if ctx.TokenPayload() != nil {
		t.Error("token payload should be nil initially")
	}
	p := &mockPayload{login: "bob"}
	ctx.SetTokenPayload(p)
	if ctx.TokenPayload().(*mockPayload).Login() != "bob" {
		t.Error("token payload not stored")
	}
}

func TestContext_VisitUserValues(t *testing.T) {
	fctx := &fasthttp.RequestCtx{}
	fctx.SetUserValue("a", 1)
	fctx.SetUserValue("b", 2)
	ctx := NewContext(fctx)

	count := 0
	ctx.VisitUserValues(func(_ []byte, _ interface{}) { count++ })
	if count != 2 {
		t.Errorf("want 2 user values, got %d", count)
	}
}

func TestContext_RequestBody(t *testing.T) {
	fctx := &fasthttp.RequestCtx{}
	fctx.Request.SetBody([]byte(`{"x":1}`))
	ctx := NewContext(fctx)
	if string(ctx.RequestBody()) != `{"x":1}` {
		t.Errorf("unexpected body: %s", ctx.RequestBody())
	}
}

func TestContext_GoContext(t *testing.T) {
	fctx := &fasthttp.RequestCtx{}
	ctx := NewContext(fctx)
	if ctx.Context() == nil {
		t.Error("Context() should never return nil")
	}
}

// ── Func adapter ──────────────────────────────────────────────────────────────

func TestFunc_Adapter(t *testing.T) {
	called := false
	v := noAuthVatel()
	fctx := newGETCtx("/ping")

	run(v, GET("/ping", Func(func(ctx Context) error {
		called = true
		return nil
	})), fctx)

	if !called {
		t.Error("Func-wrapped handler not called")
	}
	if responseStatus(fctx) != 200 {
		t.Errorf("want 200, got %d", responseStatus(fctx))
	}
}

func TestFunc_Adapter_Error(t *testing.T) {
	v := noAuthVatel()
	fctx := newGETCtx("/ping")

	run(v, GET("/ping", Func(func(ctx Context) error {
		return ErrValidationFailed.New()
	})), fctx)

	if responseStatus(fctx) != 400 {
		t.Errorf("want 400, got %d", responseStatus(fctx))
	}
}

// ── orderTracker helpers ──────────────────────────────────────────────────────

type orderTracker struct {
	order *[]string
	label string
	inner Handler
}

func (t *orderTracker) Handle(ctx Context) error {
	*t.order = append(*t.order, t.label)
	return t.inner.Handle(ctx)
}

// ── optional body (WithOptionalBody) ─────────────────────────────────────────

func TestHandler_OptionalBody(t *testing.T) {
	tests := []struct {
		name       string
		body       []byte
		wantStatus int
		wantCalled bool
		wantTitle  string
		wantCount  int
	}{
		{name: "nil body", body: nil, wantStatus: 200, wantCalled: true},
		{name: "empty body", body: []byte{}, wantStatus: 200, wantCalled: true},
		{name: "whitespace body", body: []byte(" \r\n\t "), wantStatus: 200, wantCalled: true},
		{name: "json body", body: []byte(`{"title":"x","count":2}`), wantStatus: 200, wantCalled: true, wantTitle: "x", wantCount: 2},
		{name: "invalid json", body: []byte(`not json`), wantStatus: 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &bodyHandler{}
			fctx := newPOSTCtx("/items", tt.body)

			run(noAuthVatel(), POST("/items", func() Handler { return h }, WithOptionalBody()), fctx)

			if got := responseStatus(fctx); got != tt.wantStatus {
				t.Fatalf("want %d, got %d (body %s)", tt.wantStatus, got, fctx.Response.Body())
			}
			if h.called != tt.wantCalled {
				t.Fatalf("handler called = %v, want %v", h.called, tt.wantCalled)
			}
			if h.in.Title != tt.wantTitle || h.in.Count != tt.wantCount {
				t.Errorf("unexpected input: %+v", h.in)
			}
		})
	}
}

func TestHandler_WithoutOptionalBody_BlankBodyRejected(t *testing.T) {
	for _, body := range [][]byte{nil, []byte("  \n ")} {
		h := &bodyHandler{}
		fctx := newPOSTCtx("/items", body)

		run(noAuthVatel(), POST("/items", func() Handler { return h }), fctx)

		if got := responseStatus(fctx); got != 400 {
			t.Errorf("body %q: want 400, got %d", body, got)
		}
		if h.called {
			t.Errorf("body %q: handler must not be called", body)
		}
	}
}

func TestCompile_OptionalBodyOnMethodsWithoutBody(t *testing.T) {
	ctrl := func() Handler { return &plainHandler{} }
	for _, e := range []Endpoint{
		GET("/items", ctrl, WithOptionalBody()),
		DELETE("/items", ctrl, WithOptionalBody()),
		HEAD("/items", ctrl, WithOptionalBody()),
		OPTIONS("/items", ctrl, WithOptionalBody()),
	} {
		if err := e.compile(noAuthVatel()); err == nil {
			t.Errorf("%s: expected compile error for WithOptionalBody", e.Method)
		}
	}
}

// countingInput is a body input whose Validate() counts calls and rejects
// an empty Name.
type countingInput struct {
	Name  string `json:"name"`
	calls int
}

func (c *countingInput) Validate() error {
	c.calls++
	if c.Name == "" {
		return ErrValidationFailed.New().Msg("name is required")
	}
	return nil
}

type validatedBodyHandler struct {
	in     countingInput
	called bool
}

func (h *validatedBodyHandler) Input() interface{}       { return &h.in }
func (h *validatedBodyHandler) Handle(ctx Context) error { h.called = true; return nil }

func TestHandler_OptionalBody_Validator(t *testing.T) {
	tests := []struct {
		name       string
		body       []byte
		wantStatus int
		wantCalls  int
	}{
		{name: "empty body skips validator", body: nil, wantStatus: 200, wantCalls: 0},
		{name: "valid body", body: []byte(`{"name":"a"}`), wantStatus: 200, wantCalls: 1},
		{name: "invalid body", body: []byte(`{"name":""}`), wantStatus: 400, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &validatedBodyHandler{}
			fctx := newPOSTCtx("/items", tt.body)

			run(noAuthVatel(), POST("/items", func() Handler { return h }, WithOptionalBody()), fctx)

			if got := responseStatus(fctx); got != tt.wantStatus {
				t.Fatalf("want %d, got %d", tt.wantStatus, got)
			}
			if h.in.calls != tt.wantCalls {
				t.Errorf("Validate calls = %d, want %d", h.in.calls, tt.wantCalls)
			}
			if h.called != (tt.wantStatus == 200) {
				t.Errorf("handler called = %v", h.called)
			}
		})
	}
}

// ── Querier: query separate from body ─────────────────────────────────────────

type pageQuery struct {
	Page int    `param:"page"`
	Sort string `param:"sort"`
}

type titleBody struct {
	Title string `json:"title"`
}

// queryAndBodyHandler embeds both QueryParams and Body.
type queryAndBodyHandler struct {
	QueryParams[pageQuery]
	Body[titleBody]
	called bool
}

func (h *queryAndBodyHandler) Handle(Context) error { h.called = true; return nil }

// queryOnlyHandler embeds only QueryParams.
type queryOnlyHandler struct {
	QueryParams[pageQuery]
	called bool
}

func (h *queryOnlyHandler) Handle(Context) error { h.called = true; return nil }

func TestHandler_QueryParamsAndBody_POST(t *testing.T) {
	h := &queryAndBodyHandler{}
	fctx := newPOSTCtx("/items?page=2&sort=asc", []byte(`{"title":"x"}`))

	run(noAuthVatel(), POST("/items", func() Handler { return h }), fctx)

	if got := responseStatus(fctx); got != 200 {
		t.Fatalf("want 200, got %d (body %s)", got, fctx.Response.Body())
	}
	if h.Query.Page != 2 || h.Query.Sort != "asc" {
		t.Errorf("query not decoded: %+v", h.Query)
	}
	if h.In.Title != "x" {
		t.Errorf("body not decoded: %+v", h.In)
	}
}

func TestHandler_QueryParamsOnly_POSTNeedsNoBody(t *testing.T) {
	h := &queryOnlyHandler{}
	fctx := newPOSTCtx("/items?page=4", nil)

	run(noAuthVatel(), POST("/items", func() Handler { return h }), fctx)

	if got := responseStatus(fctx); got != 200 {
		t.Fatalf("want 200, got %d (body %s)", got, fctx.Response.Body())
	}
	if h.Query.Page != 4 {
		t.Errorf("query not decoded: %+v", h.Query)
	}
}

func TestHandler_QueryParams_GETAndDELETE(t *testing.T) {
	for _, method := range []string{"GET", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			h := &queryOnlyHandler{}
			fctx := newGETCtx("/items?page=5&sort=desc")
			fctx.Request.Header.SetMethod(method)

			ep := buildEndpoint(method, "/items", func() Handler { return h })
			run(noAuthVatel(), ep, fctx)

			if got := responseStatus(fctx); got != 200 {
				t.Fatalf("want 200, got %d", got)
			}
			if h.Query.Page != 5 || h.Query.Sort != "desc" {
				t.Errorf("query not decoded: %+v", h.Query)
			}
		})
	}
}

// legacyInputHandler is a v0.14-style controller: a single Input() whose struct
// has both query (param) and body (json) tags.
type legacyInputHandler struct {
	in struct {
		Page  int    `param:"page" json:"-"`
		Title string `json:"title"`
	}
	called bool
}

func (h *legacyInputHandler) Input() interface{}   { return &h.in }
func (h *legacyInputHandler) Handle(Context) error { h.called = true; return nil }

func TestHandler_LegacyInputer_GETQuery_POSTBody(t *testing.T) {
	t.Run("GET decodes query", func(t *testing.T) {
		h := &legacyInputHandler{}
		fctx := newGETCtx("/items?page=9")
		run(noAuthVatel(), GET("/items", func() Handler { return h }), fctx)
		if got := responseStatus(fctx); got != 200 {
			t.Fatalf("want 200, got %d", got)
		}
		if h.in.Page != 9 {
			t.Errorf("Page: want 9, got %d", h.in.Page)
		}
	})
	t.Run("POST decodes body, not query", func(t *testing.T) {
		h := &legacyInputHandler{}
		fctx := newPOSTCtx("/items?page=9", []byte(`{"title":"t"}`))
		run(noAuthVatel(), POST("/items", func() Handler { return h }), fctx)
		if got := responseStatus(fctx); got != 200 {
			t.Fatalf("want 200, got %d", got)
		}
		if h.in.Title != "t" {
			t.Errorf("Title: want t, got %q", h.in.Title)
		}
		if h.in.Page != 0 {
			t.Errorf("Page must not be decoded from query on POST, got %d", h.in.Page)
		}
	})
}

// validatedQuery rejects a non-positive Page and counts Validate calls.
type validatedQuery struct {
	Page  int `param:"page"`
	calls int
}

func (q *validatedQuery) Validate() error {
	q.calls++
	if q.Page <= 0 {
		return ErrValidationFailed.New().Msg("page must be positive")
	}
	return nil
}

type validatedQuerierHandler struct {
	QueryParams[validatedQuery]
	called bool
}

func (h *validatedQuerierHandler) Handle(Context) error { h.called = true; return nil }

type validatedLegacyQueryHandler struct {
	in     validatedQuery
	called bool
}

func (h *validatedLegacyQueryHandler) Input() interface{}   { return &h.in }
func (h *validatedLegacyQueryHandler) Handle(Context) error { h.called = true; return nil }

type validatedQueryAndBodyHandler struct {
	QueryParams[validatedQuery]
	Body[countingInput]
	called bool
}

func (h *validatedQueryAndBodyHandler) Handle(Context) error { h.called = true; return nil }

func TestHandler_Validator_QuerierOnly(t *testing.T) {
	for _, tt := range []struct {
		uri        string
		wantStatus int
	}{
		{"/items?page=1", 200},
		{"/items?page=0", 400},
	} {
		h := &validatedQuerierHandler{}
		fctx := newGETCtx(tt.uri)
		run(noAuthVatel(), GET("/items", func() Handler { return h }), fctx)
		if got := responseStatus(fctx); got != tt.wantStatus {
			t.Errorf("%s: want %d, got %d", tt.uri, tt.wantStatus, got)
		}
		if h.Query.calls != 1 {
			t.Errorf("%s: Validate calls = %d, want 1", tt.uri, h.Query.calls)
		}
		if h.called != (tt.wantStatus == 200) {
			t.Errorf("%s: handler called = %v", tt.uri, h.called)
		}
	}
}

func TestHandler_Validator_LegacyInputerQuery(t *testing.T) {
	for _, tt := range []struct {
		uri        string
		wantStatus int
	}{
		{"/items?page=1", 200},
		{"/items?page=0", 400},
	} {
		h := &validatedLegacyQueryHandler{}
		fctx := newGETCtx(tt.uri)
		run(noAuthVatel(), GET("/items", func() Handler { return h }), fctx)
		if got := responseStatus(fctx); got != tt.wantStatus {
			t.Errorf("%s: want %d, got %d", tt.uri, tt.wantStatus, got)
		}
		if h.in.calls != 1 {
			t.Errorf("%s: Validate calls = %d, want 1", tt.uri, h.in.calls)
		}
		if h.called != (tt.wantStatus == 200) {
			t.Errorf("%s: handler called = %v", tt.uri, h.called)
		}
	}
}

func TestHandler_Validator_QueryBeforeBody(t *testing.T) {
	tests := []struct {
		name           string
		uri            string
		body           string
		wantStatus     int
		wantQueryCalls int
		wantBodyCalls  int
	}{
		{"both valid", "/items?page=1", `{"name":"a"}`, 200, 1, 1},
		{"query invalid skips body validation", "/items?page=0", `{"name":"a"}`, 400, 1, 0},
		{"body invalid", "/items?page=1", `{"name":""}`, 400, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &validatedQueryAndBodyHandler{}
			fctx := newPOSTCtx(tt.uri, []byte(tt.body))
			run(noAuthVatel(), POST("/items", func() Handler { return h }), fctx)
			if got := responseStatus(fctx); got != tt.wantStatus {
				t.Fatalf("want %d, got %d", tt.wantStatus, got)
			}
			if h.Query.calls != tt.wantQueryCalls {
				t.Errorf("query Validate calls = %d, want %d", h.Query.calls, tt.wantQueryCalls)
			}
			if h.In.calls != tt.wantBodyCalls {
				t.Errorf("body Validate calls = %d, want %d", h.In.calls, tt.wantBodyCalls)
			}
			if h.In.Name != "a" && tt.body == `{"name":"a"}` {
				t.Errorf("body must be decoded even when query validation fails, got %+v", h.In)
			}
			if h.called != (tt.wantStatus == 200) {
				t.Errorf("handler called = %v", h.called)
			}
		})
	}
}

// runLogged compiles e and invokes its handler with a logger writing to buf.
func runLogged(t *testing.T, v *Vatel, e Endpoint, fctx *fasthttp.RequestCtx, buf *bytes.Buffer) {
	t.Helper()
	if err := e.compile(v); err != nil {
		t.Fatalf("compile: %v", err)
	}
	l := zerolog.New(buf)
	e.handler(&l)(fctx)
}

// lastLogLine decodes the last JSON log line from buf.
func lastLogLine(t *testing.T, buf *bytes.Buffer) map[string]interface{} {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	var m map[string]interface{}
	if err := json.Unmarshal(lines[len(lines)-1], &m); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, buf.String())
	}
	return m
}

func TestHandler_Log_QuerierUnderReqQuery_InputerUnderReqInput(t *testing.T) {
	var buf bytes.Buffer
	h := &queryAndBodyHandler{}
	fctx := newPOSTCtx("/items?page=2", []byte(`{"title":"x"}`))

	runLogged(t, noAuthVatel(), POST("/items", func() Handler { return h },
		WithLogOption(LogExit|LogReqInput)), fctx, &buf)

	m := lastLogLine(t, &buf)
	q, ok := m["reqQuery"].(map[string]interface{})
	if !ok {
		t.Fatalf("reqQuery missing in log: %s", buf.String())
	}
	if q["Page"] != float64(2) {
		t.Errorf("reqQuery.Page = %v, want 2", q["Page"])
	}
	in, ok := m["reqInput"].(map[string]interface{})
	if !ok {
		t.Fatalf("reqInput missing in log: %s", buf.String())
	}
	if in["title"] != "x" {
		t.Errorf("reqInput.title = %v, want x", in["title"])
	}
}

// ── HEAD and OPTIONS ──────────────────────────────────────────────────────────

type itemResultHandler struct {
	Output[struct {
		Name string `json:"name"`
	}]
	called bool
}

func (h *itemResultHandler) Handle(Context) error {
	h.called = true
	h.Out.Name = "x"
	return nil
}

// rawRoundTrip sends a raw HTTP/1.1 request through an in-memory fasthttp
// server and returns the response head and the bytes that followed it.
func rawRoundTrip(t *testing.T, h fasthttp.RequestHandler, method, uri string) (head, body string) {
	t.Helper()
	ln := fasthttputil.NewInmemoryListener()
	srv := &fasthttp.Server{Handler: h}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})

	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	req := method + " " + uri + " HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	parts := strings.SplitN(string(raw), "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatalf("malformed response: %q", raw)
	}
	return parts[0], parts[1]
}

func TestHEAD_InMemoryServer_NoBody(t *testing.T) {
	v := noAuthVatel()
	headH := &itemResultHandler{}
	v.Add(Endpoints{
		GET("/items", func() Handler { return &itemResultHandler{} }),
		HEAD("/items", func() Handler { return headH }),
	})
	mux := router.New()
	if err := v.BuildHandlers(mux, discardLogger()); err != nil {
		t.Fatalf("build: %v", err)
	}

	// GET on the same path proves the handler produces a body.
	_, getBody := rawRoundTrip(t, mux.Handler, "GET", "/items")
	if getBody != `{"name":"x"}` {
		t.Fatalf("GET body = %q", getBody)
	}

	head, body := rawRoundTrip(t, mux.Handler, "HEAD", "/items")
	if !strings.HasPrefix(head, "HTTP/1.1 200") {
		t.Errorf("want 200, got head %q", head)
	}
	if !strings.Contains(head, "Content-Type: application/json") {
		t.Errorf("Content-Type header missing: %q", head)
	}
	if body != "" {
		t.Errorf("HEAD response must have no body, got %q", body)
	}
	if !headH.called {
		t.Error("HEAD handler was not called")
	}
}

func TestOPTIONS_CompilesAndServes(t *testing.T) {
	h := &plainHandler{}
	fctx := newGETCtx("/items")
	fctx.Request.Header.SetMethod("OPTIONS")

	run(noAuthVatel(), OPTIONS("/items", func() Handler { return h }), fctx)

	if got := responseStatus(fctx); got != 200 {
		t.Fatalf("want 200, got %d (body %s)", got, fctx.Response.Body())
	}
	if !h.called {
		t.Error("OPTIONS handler was not called")
	}
}

func TestHandler_InputRouting_AllMethods(t *testing.T) {
	builders := map[string]func(string, func() Handler, ...EndpointOption) Endpoint{
		"GET": GET, "POST": POST, "PUT": PUT, "PATCH": PATCH,
		"DELETE": DELETE, "HEAD": HEAD, "OPTIONS": OPTIONS,
	}
	queryMethods := map[string]bool{"GET": true, "DELETE": true, "HEAD": true, "OPTIONS": true}

	for method, build := range builders {
		t.Run(method+"/Querier", func(t *testing.T) {
			h := &queryOnlyHandler{}
			fctx := newPOSTCtx("/items?page=3&sort=asc", nil)
			fctx.Request.Header.SetMethod(method)

			run(noAuthVatel(), build("/items", func() Handler { return h }), fctx)

			if got := responseStatus(fctx); got != 200 {
				t.Fatalf("want 200, got %d (body %s)", got, fctx.Response.Body())
			}
			if h.Query.Page != 3 || h.Query.Sort != "asc" {
				t.Errorf("query not decoded: %+v", h.Query)
			}
		})
		t.Run(method+"/Inputer", func(t *testing.T) {
			h := &legacyInputHandler{}
			fctx := newPOSTCtx("/items?page=9", []byte(`{"title":"t"}`))
			fctx.Request.Header.SetMethod(method)

			run(noAuthVatel(), build("/items", func() Handler { return h }), fctx)

			if got := responseStatus(fctx); got != 200 {
				t.Fatalf("want 200, got %d (body %s)", got, fctx.Response.Body())
			}
			if queryMethods[method] {
				if h.in.Page != 9 || h.in.Title != "" {
					t.Errorf("want query decoded and body ignored, got %+v", h.in)
				}
				return
			}
			if h.in.Title != "t" || h.in.Page != 0 {
				t.Errorf("want body decoded and query ignored, got %+v", h.in)
			}
		})
	}
}

func TestRouter_GlobalOPTIONS_WithVatelEndpoints(t *testing.T) {
	explicit := &plainHandler{}
	v := noAuthVatel()
	v.Add(Endpoints{
		GET("/items", func() Handler { return &plainHandler{} }),
		GET("/explicit", func() Handler { return &plainHandler{} }),
		OPTIONS("/explicit", func() Handler { return explicit }),
	})
	mux := router.New()
	globalCalls := 0
	mux.GlobalOPTIONS = func(ctx *fasthttp.RequestCtx) {
		globalCalls++
		ctx.Response.Header.Set("Access-Control-Allow-Origin", "*")
		ctx.SetStatusCode(fasthttp.StatusNoContent)
	}
	if err := v.BuildHandlers(mux, discardLogger()); err != nil {
		t.Fatalf("build: %v", err)
	}

	options := func(uri string) *fasthttp.RequestCtx {
		fctx := newGETCtx(uri)
		fctx.Request.Header.SetMethod("OPTIONS")
		mux.Handler(fctx)
		return fctx
	}

	t.Run("registered path uses GlobalOPTIONS", func(t *testing.T) {
		fctx := options("/items")
		if got := responseStatus(fctx); got != fasthttp.StatusNoContent {
			t.Errorf("want 204, got %d", got)
		}
		if got := string(fctx.Response.Header.Peek("Access-Control-Allow-Origin")); got != "*" {
			t.Errorf("CORS header = %q", got)
		}
		if globalCalls != 1 {
			t.Errorf("GlobalOPTIONS calls = %d, want 1", globalCalls)
		}
	})
	t.Run("unknown path is 404", func(t *testing.T) {
		fctx := options("/unknown")
		if got := responseStatus(fctx); got != fasthttp.StatusNotFound {
			t.Errorf("want 404, got %d", got)
		}
		if globalCalls != 1 {
			t.Errorf("GlobalOPTIONS must not be called for an unknown path, calls = %d", globalCalls)
		}
	})
	t.Run("explicit vatel.OPTIONS replaces GlobalOPTIONS", func(t *testing.T) {
		fctx := options("/explicit")
		if got := responseStatus(fctx); got != 200 {
			t.Errorf("want 200, got %d", got)
		}
		if !explicit.called {
			t.Error("explicit OPTIONS handler was not called")
		}
		if globalCalls != 1 {
			t.Errorf("GlobalOPTIONS must not be called, calls = %d", globalCalls)
		}
	})
}

// ── masking of logged input and output ────────────────────────────────────────

type secretItem struct {
	Name     string `json:"name"`
	Password string `json:"password" mask:"secret"`
}

type maskNode struct {
	Name     string     `json:"name"`
	Secret   string     `json:"secret" mask:"secret"`
	Children []maskNode `json:"children"`
	Next     *maskNode  `json:"next"`
}

type sliceBodyHandler struct{ Body[[]secretItem] }

func (h *sliceBodyHandler) Handle(Context) error { return nil }

type sliceOutputHandler struct{ Output[[]secretItem] }

func (h *sliceOutputHandler) Handle(Context) error {
	h.Out = []secretItem{{Name: "a", Password: "p1"}, {Name: "b", Password: "p2"}}
	return nil
}

type stringOutputHandler struct{ Output[string] }

func (h *stringOutputHandler) Handle(Context) error { h.Out = "x"; return nil }

type optionalSecretHandler struct{ OptionalOutput[secretItem] }

func (h *optionalSecretHandler) Handle(Context) error {
	h.Out = &secretItem{Name: "a", Password: "p1"}
	return nil
}

type recursiveOutputHandler struct{ Output[maskNode] }

func (h *recursiveOutputHandler) Handle(Context) error {
	h.Out = maskNode{Name: "root", Secret: "s0", Children: []maskNode{{Name: "c", Secret: "s1"}}, Next: &maskNode{Name: "n", Secret: "s2"}}
	return nil
}

type secretBodyHandler struct {
	Body[secretItem]
	Output[secretItem]
}

func (h *secretBodyHandler) Handle(Context) error {
	h.Out = h.In
	return nil
}

type secretQueryHandler struct {
	QueryParams[struct {
		Name  string `param:"name"`
		Token string `param:"token" mask:"secret"`
	}]
}

func (h *secretQueryHandler) Handle(Context) error { return nil }

type secretGetInputHandler struct {
	Body[struct {
		Token string `param:"token" mask:"secret"`
	}]
}

func (h *secretGetInputHandler) Handle(Context) error { return nil }

type plainItem struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

type plainBodyHandler struct {
	Body[plainItem]
	Output[plainItem]
}

func (h *plainBodyHandler) Handle(Context) error {
	h.Out = h.In
	return nil
}

type chanOutputHandler struct {
	Output[struct {
		Name string   `json:"name"`
		C    chan int `json:"c"`
	}]
}

func (h *chanOutputHandler) Handle(Context) error {
	h.Out.Name = "leak"
	h.Out.C = make(chan int)
	return nil
}

func TestCompile_DefaultMasker_NoPanic(t *testing.T) {
	tests := []struct {
		name string
		e    Endpoint
	}{
		{"Output[[]T]", GET("/a", func() Handler { return &sliceOutputHandler{} })},
		{"Output[string]", GET("/a", func() Handler { return &stringOutputHandler{} })},
		{"OptionalOutput[T]", GET("/a", func() Handler { return &optionalSecretHandler{} })},
		{"Body[[]T]", POST("/a", func() Handler { return &sliceBodyHandler{} })},
		{"recursive", GET("/a", func() Handler { return &recursiveOutputHandler{} })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := tt.e
			if err := e.compile(noAuthVatel()); err != nil {
				t.Fatalf("compile: %v", err)
			}
			if e.jm == nil {
				t.Error("default masker is not set")
			}
		})
	}
}

// maskedValues returns the values of key in every element of the JSON array
// found in the log line under logKey.
func maskedValues(t *testing.T, m map[string]interface{}, logKey, key string) []interface{} {
	t.Helper()
	arr, ok := m[logKey].([]interface{})
	if !ok {
		t.Fatalf("%s is not an array in log: %v", logKey, m)
	}
	var res []interface{}
	for _, el := range arr {
		res = append(res, el.(map[string]interface{})[key])
	}
	return res
}

func TestHandler_Log_MasksArrayElements(t *testing.T) {
	t.Run("Body[[]T]", func(t *testing.T) {
		var buf bytes.Buffer
		fctx := newPOSTCtx("/items", []byte(`[{"name":"a","password":"p1"},{"name":"b","password":"p2"}]`))
		runLogged(t, noAuthVatel(), POST("/items", func() Handler { return &sliceBodyHandler{} },
			WithLogOption(LogExit|LogReqBody|LogReqInput)), fctx, &buf)

		m := lastLogLine(t, &buf)
		for _, key := range []string{"requestBody", "reqInput"} {
			for i, v := range maskedValues(t, m, key, "password") {
				if v != jsonmask.DefaultMask {
					t.Errorf("%s[%d].password = %v, want %q", key, i, v, jsonmask.DefaultMask)
				}
			}
		}
	})
	t.Run("Output[[]T]", func(t *testing.T) {
		var buf bytes.Buffer
		fctx := newGETCtx("/items")
		runLogged(t, noAuthVatel(), GET("/items", func() Handler { return &sliceOutputHandler{} },
			WithLogOption(LogExit|LogRespBody|LogRespOutput)), fctx, &buf)

		m := lastLogLine(t, &buf)
		for _, key := range []string{"respBody", "result"} {
			vals := maskedValues(t, m, key, "password")
			if len(vals) != 2 {
				t.Fatalf("%s: want 2 elements, got %d", key, len(vals))
			}
			for i, v := range vals {
				if v != jsonmask.DefaultMask {
					t.Errorf("%s[%d].password = %v, want %q", key, i, v, jsonmask.DefaultMask)
				}
			}
		}
		if !strings.Contains(string(fctx.Response.Body()), `"password":"p1"`) {
			t.Errorf("response body must not be masked: %s", fctx.Response.Body())
		}
	})
}

func TestHandler_Log_MasksTaggedFields(t *testing.T) {
	maskers := []struct {
		name string
		opts []func(*Option)
	}{
		{"default masker", nil},
		{"WithJsonMasker", []func(*Option){WithJsonMasker(jsonmask.New())}},
	}
	flags := []struct {
		lo  LogOption
		key string
	}{
		{LogReqBody, "requestBody"},
		{LogReqInput, "reqInput"},
		{LogRespBody, "respBody"},
		{LogRespOutput, "result"},
	}
	for _, mk := range maskers {
		for _, f := range flags {
			t.Run(mk.name+"/"+f.key, func(t *testing.T) {
				var buf bytes.Buffer
				fctx := newPOSTCtx("/items", []byte(`{"name":"a","password":"p1"}`))
				runLogged(t, noAuthVatel(mk.opts...), POST("/items", func() Handler { return &secretBodyHandler{} },
					WithLogOption(LogExit|f.lo)), fctx, &buf)

				m := lastLogLine(t, &buf)
				obj, ok := m[f.key].(map[string]interface{})
				if !ok {
					t.Fatalf("%s missing in log: %s", f.key, buf.String())
				}
				if obj["password"] != jsonmask.DefaultMask || obj["name"] != "a" {
					t.Errorf("%s = %v, want password masked and name kept", f.key, obj)
				}
				if strings.Contains(buf.String(), "p1") {
					t.Errorf("secret leaked to log: %s", buf.String())
				}
				for _, old := range []string{"maskedRequestBody", "maskedRespBody"} {
					if _, ok := m[old]; ok {
						t.Errorf("unexpected log key %s", old)
					}
				}
			})
		}
	}
}

func TestHandler_Log_MasksOptionalOutput(t *testing.T) {
	var buf bytes.Buffer
	fctx := newGETCtx("/items")
	runLogged(t, noAuthVatel(), GET("/items", func() Handler { return &optionalSecretHandler{} },
		WithLogOption(LogExit|LogRespBody|LogRespOutput)), fctx, &buf)

	if strings.Contains(buf.String(), "p1") {
		t.Errorf("secret leaked to log: %s", buf.String())
	}
	m := lastLogLine(t, &buf)
	if obj, _ := m["respBody"].(map[string]interface{}); obj["password"] != jsonmask.DefaultMask {
		t.Errorf("respBody = %v, want password masked", m["respBody"])
	}
}

func TestHandler_Log_MasksQueryInputs(t *testing.T) {
	t.Run("Querier", func(t *testing.T) {
		var buf bytes.Buffer
		fctx := newGETCtx("/items?name=a&token=t1")
		runLogged(t, noAuthVatel(), GET("/items", func() Handler { return &secretQueryHandler{} },
			WithLogOption(LogExit|LogReqInput)), fctx, &buf)

		m := lastLogLine(t, &buf)
		q, _ := m["reqQuery"].(map[string]interface{})
		if q["Token"] != jsonmask.DefaultMask || q["Name"] != "a" {
			t.Errorf("reqQuery = %v, want Token masked and Name kept", m["reqQuery"])
		}
	})
	t.Run("Inputer on GET", func(t *testing.T) {
		var buf bytes.Buffer
		fctx := newGETCtx("/items?token=t1")
		runLogged(t, noAuthVatel(), GET("/items", func() Handler { return &secretGetInputHandler{} },
			WithLogOption(LogExit|LogReqInput)), fctx, &buf)

		m := lastLogLine(t, &buf)
		in, _ := m["reqInput"].(map[string]interface{})
		if in["Token"] != jsonmask.DefaultMask {
			t.Errorf("reqInput = %v, want Token masked", m["reqInput"])
		}
	})
}

func TestHandler_Log_NoMaskTags_Unchanged(t *testing.T) {
	var buf bytes.Buffer
	fctx := newPOSTCtx("/items", []byte(`{"name":"a", "password":"p1"}`))
	runLogged(t, noAuthVatel(), POST("/items", func() Handler { return &plainBodyHandler{} },
		WithLogOption(LogExit|LogReqBody|LogReqInput|LogRespBody|LogRespOutput)), fctx, &buf)

	m := lastLogLine(t, &buf)
	for _, key := range []string{"requestBody", "reqInput", "respBody", "result"} {
		obj, ok := m[key].(map[string]interface{})
		if !ok {
			t.Fatalf("%s missing in log: %s", key, buf.String())
		}
		if obj["name"] != "a" || obj["password"] != "p1" {
			t.Errorf("%s = %v, want original values", key, obj)
		}
	}
}

func TestHandler_Log_MarshalErrorOmitsResult(t *testing.T) {
	var buf bytes.Buffer
	fctx := newGETCtx("/items")
	runLogged(t, noAuthVatel(), GET("/items", func() Handler { return &chanOutputHandler{} },
		WithLogOption(LogExit|LogRespBody|LogRespOutput)), fctx, &buf)

	if got := responseStatus(fctx); got != 500 {
		t.Errorf("want 500, got %d", got)
	}
	m := lastLogLine(t, &buf)
	if _, ok := m["err"]; !ok {
		t.Errorf("error missing in log: %s", buf.String())
	}
	for _, key := range []string{"result", "respBody"} {
		if _, ok := m[key]; ok {
			t.Errorf("unexpected %s in log: %s", key, buf.String())
		}
	}
	if strings.Contains(buf.String(), "leak") {
		t.Errorf("response object leaked to log: %s", buf.String())
	}
}

// ── request context: Shutdown and timeout ─────────────────────────────────────

// ctxWaitHandler signals started, then waits until its request context is
// done (or a safety timeout expires), sends the context error to errc and
// returns it.
type ctxWaitHandler struct {
	started chan struct{}
	errc    chan error
}

func newCtxWaitHandler() *ctxWaitHandler {
	return &ctxWaitHandler{started: make(chan struct{}), errc: make(chan error, 1)}
}

func (h *ctxWaitHandler) Handle(ctx Context) error {
	close(h.started)
	var err error
	select {
	case <-ctx.Context().Done():
		err = ctx.Context().Err()
	case <-time.After(10 * time.Second):
		err = fmt.Errorf("request context was not cancelled")
	}
	h.errc <- err
	return err
}

func TestContext_ShutdownCancelsHandlerContext(t *testing.T) {
	v := noAuthVatel()
	h := newCtxWaitHandler()
	fctx := newGETCtx("/wait")

	done := make(chan struct{})
	go func() {
		defer close(done)
		run(v, GET("/wait", func() Handler { return h }), fctx)
	}()
	<-h.started

	if err := v.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	<-done

	if err := <-h.errc; !stderrors.Is(err, context.Canceled) {
		t.Errorf("handler context error = %v, want context.Canceled", err)
	}
	if got := responseStatus(fctx); got != 503 {
		t.Errorf("want 503, got %d", got)
	}
	if err := v.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

func TestContext_ShutdownWithServer_CancelsAfterDeadline(t *testing.T) {
	v := noAuthVatel()
	h := newCtxWaitHandler()
	v.Add(Endpoints{GET("/wait", func() Handler { return h })})
	mux := router.New()
	if err := v.BuildHandlers(mux, discardLogger()); err != nil {
		t.Fatalf("BuildHandlers: %v", err)
	}

	ln := fasthttputil.NewInmemoryListener()
	v.srv = &fasthttp.Server{Handler: mux.Handler}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = v.srv.Serve(ln)
	}()

	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("GET /wait HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	<-h.started

	// The in-flight request keeps waiting on its context, so the server cannot
	// stop before the shutdown deadline; then the base context is cancelled.
	sctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := v.Shutdown(sctx); !stderrors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown = %v, want context.DeadlineExceeded", err)
	}

	raw, _ := io.ReadAll(conn)
	if !strings.HasPrefix(string(raw), "HTTP/1.1 503") {
		t.Errorf("response = %q, want 503", raw)
	}
	<-served
	if err := <-h.errc; !stderrors.Is(err, context.Canceled) {
		t.Errorf("handler context error = %v, want context.Canceled", err)
	}
}

func TestContext_EndpointTimeout(t *testing.T) {
	h := newCtxWaitHandler()
	var deadlineSet bool
	fctx := newGETCtx("/wait")

	run(noAuthVatel(), GET("/wait", Func(func(ctx Context) error {
		_, deadlineSet = ctx.Context().Deadline()
		return h.Handle(ctx)
	}), WithTimeout(50*time.Millisecond)), fctx)

	if !deadlineSet {
		t.Error("Deadline() not set")
	}
	if err := <-h.errc; !stderrors.Is(err, context.DeadlineExceeded) {
		t.Errorf("handler context error = %v, want context.DeadlineExceeded", err)
	}
	if got := responseStatus(fctx); got != 504 {
		t.Errorf("want 504, got %d", got)
	}
}

func TestContext_DefaultTimeout(t *testing.T) {
	deadline := func(v *Vatel, opts ...EndpointOption) (time.Duration, bool) {
		var d time.Duration
		var ok bool
		run(v, GET("/d", Func(func(ctx Context) error {
			var dl time.Time
			dl, ok = ctx.Context().Deadline()
			d = time.Until(dl)
			return nil
		}), opts...), newGETCtx("/d"))
		return d, ok
	}

	v := noAuthVatel(WithDefaultTimeout(time.Hour))

	d, ok := deadline(v)
	if !ok || d <= 30*time.Minute || d > time.Hour {
		t.Errorf("default timeout: deadline in %v (set %v), want about 1h", d, ok)
	}
	d, ok = deadline(v, WithTimeout(3*time.Hour))
	if !ok || d <= 2*time.Hour {
		t.Errorf("own timeout: deadline in %v (set %v), want about 3h", d, ok)
	}
}

func TestContext_TimeoutCancelledAfterRequest(t *testing.T) {
	var reqCtx context.Context
	run(noAuthVatel(), GET("/d", Func(func(ctx Context) error {
		reqCtx = ctx.Context()
		return nil
	}), WithTimeout(time.Hour)), newGETCtx("/d"))

	if !stderrors.Is(reqCtx.Err(), context.Canceled) {
		t.Errorf("request context error after request = %v, want context.Canceled", reqCtx.Err())
	}
}

func TestContext_NoTimeout_NoDeadline(t *testing.T) {
	var reqCtx context.Context
	var deadlineSet bool
	run(noAuthVatel(), GET("/d", Func(func(ctx Context) error {
		reqCtx = ctx.Context()
		_, deadlineSet = reqCtx.Deadline()
		return nil
	})), newGETCtx("/d"))

	if deadlineSet {
		t.Error("Deadline() set without a timeout")
	}
	if reqCtx.Err() != nil {
		t.Errorf("request context error = %v, want nil before Shutdown", reqCtx.Err())
	}

	ctx := NewContext(&fasthttp.RequestCtx{})
	if ctx.Context() == nil || ctx.Context().Err() != nil {
		t.Errorf("NewContext: Context() = %v", ctx.Context())
	}
	if _, ok := ctx.Context().Deadline(); ok {
		t.Error("NewContext: Deadline() set")
	}
}

func TestContext_ErrorStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"deadline", context.DeadlineExceeded, 504},
		{"canceled", context.Canceled, 503},
		{"wrapped deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), 504},
		{"axkit wrapped canceled", errors.Wrap(context.Canceled, "query"), 503},
		{"explicit status wins", ErrValidationFailed.Wrap(context.DeadlineExceeded), 400},
		{"plain error", fmt.Errorf("boom"), 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fctx := newGETCtx("/e")
			run(noAuthVatel(), GET("/e", Func(func(Context) error { return tt.err })), fctx)
			if got := responseStatus(fctx); got != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, got, fctx.Response.Body())
			}
		})
	}
}

// ── OnErrorResponse metadata and LogSilent ───────────────────────────────────

func TestHandler_OnErrorResponse_MetaOnAuthorizationReject(t *testing.T) {
	v := noAuthVatel()
	v.AddMiddleware(OnAuthorization, func(Context, EndpointMeta) error {
		return ErrForbidden.New()
	})
	var got EndpointMeta
	called := false
	v.AddMiddleware(OnErrorResponse, func(_ Context, m EndpointMeta) error {
		called = true
		got = m
		return nil
	})
	fctx := newGETCtx("/items")

	run(v, GET("/items", func() Handler { return &plainHandler{} }), fctx)

	if responseStatus(fctx) != 403 {
		t.Fatalf("want 403, got %d", responseStatus(fctx))
	}
	if !called {
		t.Fatal("OnErrorResponse not called")
	}
	if got.Method != "GET" || got.Path != "/items" {
		t.Errorf("OnErrorResponse meta = %+v, want GET /items", got)
	}
}

func TestHandler_LogSilent(t *testing.T) {
	type tc struct {
		name       string
		method     string
		body       string
		header     string
		lo         LogOption
		mdwErr     error // returned by an OnRequest middleware
		handlerErr error
		wantStatus int
		wantLog    bool
	}
	cases := []tc{
		{name: "success", method: "GET", lo: LogSilent, wantStatus: 200},
		{name: "silent wins over other bits", method: "GET", lo: LogSilent | LogEnter | LogExit | LogReqInput, wantStatus: 200},
		{name: "4xx from middleware", method: "GET", lo: LogSilent, mdwErr: ErrForbidden.New(), wantStatus: 403},
		{name: "4xx from decoding", method: "POST", body: `{"title":`, lo: LogSilent, wantStatus: 400},
		{name: "4xx from handler", method: "GET", lo: LogSilent, handlerErr: ErrValidationFailed.New(), wantStatus: 400},
		{name: "5xx from handler is logged", method: "GET", lo: LogSilent, handlerErr: fmt.Errorf("boom"), wantStatus: 500, wantLog: true},
		{name: "5xx from middleware is logged", method: "GET", lo: LogSilent, mdwErr: ErrInternalError.New(), wantStatus: 500, wantLog: true},
		{name: "header does not override silent", method: "GET", header: "enter,exit,req-input", lo: LogSilent, wantStatus: 200},
		{name: "header does not override silent on 4xx", method: "GET", header: "enter,exit", lo: LogSilent, handlerErr: ErrValidationFailed.New(), wantStatus: 400},
		{name: "silent set by header", method: "GET", header: "silent,exit", lo: LogFull, wantStatus: 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			v := noAuthVatel()
			if c.mdwErr != nil {
				err := c.mdwErr
				v.AddMiddleware(OnRequest, func(Context, EndpointMeta) error { return err })
			}
			var fctx *fasthttp.RequestCtx
			var ep Endpoint
			if c.method == "POST" {
				fctx = newPOSTCtx("/items", []byte(c.body))
				ep = POST("/items", func() Handler { return &bodyHandler{} }, WithLogOption(c.lo))
			} else {
				fctx = newGETCtx("/items")
				herr := c.handlerErr
				ep = GET("/items", func() Handler { return &plainHandler{retErr: herr} }, WithLogOption(c.lo))
			}
			if c.header != "" {
				fctx.Request.Header.Set("X-Set-Log-Level", c.header)
			}

			runLogged(t, v, ep, fctx, &buf)

			if responseStatus(fctx) != c.wantStatus {
				t.Fatalf("want status %d, got %d", c.wantStatus, responseStatus(fctx))
			}
			if !c.wantLog {
				if buf.Len() != 0 {
					t.Errorf("want empty log, got %s", buf.String())
				}
				return
			}
			m := lastLogLine(t, &buf)
			if m["message"] != "request failed" {
				t.Errorf("want error log line, got %s", buf.String())
			}
		})
	}
}

func TestHandler_NotSilent_4xxLogged(t *testing.T) {
	var buf bytes.Buffer
	fctx := newGETCtx("/items")

	runLogged(t, noAuthVatel(), GET("/items", func() Handler {
		return &plainHandler{retErr: ErrValidationFailed.New()}
	}, WithLogOption(LogExit)), fctx, &buf)

	if m := lastLogLine(t, &buf); m["message"] != "request failed" {
		t.Errorf("4xx must be logged without LogSilent, got %s", buf.String())
	}
}
