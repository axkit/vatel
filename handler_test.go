package vatel

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/fasthttp/router"
	"github.com/valyala/fasthttp"
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
