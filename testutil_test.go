package vatel

import (
	"io"

	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

// ── Mock implementations ─────────────────────────────────────────────────────

type mockPayload struct {
	user  int
	login string
	role  int
	perms []byte
	debug bool
}

func (m *mockPayload) User() int          { return m.user }
func (m *mockPayload) Login() string      { return m.login }
func (m *mockPayload) Role() int          { return m.role }
func (m *mockPayload) Perms() []byte      { return m.perms }
func (m *mockPayload) Extra() interface{} { return nil }
func (m *mockPayload) Debug() bool        { return m.debug }

type mockTokener struct{ payload AuthPayload }

func (m *mockTokener) SystemPayload() map[string]any { return nil }
func (m *mockTokener) ApplicationPayload() AuthPayload { return m.payload }

type mockDecoder struct {
	token Tokener
	err   error
}

func (m *mockDecoder) Decode([]byte) (Tokener, error) { return m.token, m.err }

type mockAuthorizer struct {
	allowed bool
	err     error
}

func (m *mockAuthorizer) IsAllowed([]byte, ...uint) (bool, error) { return m.allowed, m.err }

type mockRevoker struct{ revoked bool; err error }

func (m *mockRevoker) IsTokenRevoked(string) (bool, error) { return m.revoked, m.err }

type mockMetrics struct {
	lastMethod string
	lastStatus int
	callCount  int
}

func (m *mockMetrics) ReportMetric(method, path string, statusCode int, _ float64, _ int) {
	m.lastMethod = method
	m.lastStatus = statusCode
	m.callCount++
}

type mockAlarmer struct{ alarmed bool }

func (m *mockAlarmer) Alarm(error) { m.alarmed = true }

// ── Test handler types ────────────────────────────────────────────────────────

// plainHandler has no optional interfaces — just Handle.
type plainHandler struct {
	called bool
	retErr error
	ctxVal interface{} // stores ctx.Get("key") if set
}

func (h *plainHandler) Handle(ctx Context) error {
	h.called = true
	h.ctxVal = ctx.Get("key")
	return h.retErr
}

// queryHandler decodes URL query params.
type queryHandler struct {
	in struct {
		ID   int    `param:"id"`
		Name string `param:"name"`
	}
	called bool
}

func (h *queryHandler) Input() interface{}        { return &h.in }
func (h *queryHandler) Handle(ctx Context) error { h.called = true; return nil }

// bodyHandler decodes a POST body.
type bodyHandler struct {
	in struct {
		Title string `json:"title"`
		Count int    `json:"count"`
	}
	called bool
}

func (h *bodyHandler) Input() interface{}        { return &h.in }
func (h *bodyHandler) Handle(ctx Context) error { h.called = true; return nil }

// pathHandler decodes URL path parameters.
type pathHandler struct {
	param struct {
		ID int `param:"id"`
	}
	called bool
}

func (h *pathHandler) Param() interface{}        { return &h.param }
func (h *pathHandler) Handle(ctx Context) error { h.called = true; return nil }

// resultHandler produces a JSON response.
type resultHandler struct {
	out struct {
		Message string `json:"message"`
	}
}

func (h *resultHandler) Result() interface{} { return &h.out }
func (h *resultHandler) Handle(ctx Context) error {
	h.out.Message = "hello"
	return nil
}

// nilResultHandler returns nil from Result(), signalling no response body.
type nilResultHandler struct{ out *struct{ V int } }

func (h *nilResultHandler) Result() interface{} { return h.out }
func (h *nilResultHandler) Handle(ctx Context) error { return nil }

// tokenHandler records the token payload it sees.
type tokenHandler struct{ payload any }

func (h *tokenHandler) Handle(ctx Context) error {
	h.payload = ctx.TokenPayload()
	return nil
}

// ── Test helper functions ─────────────────────────────────────────────────────

// discardLogger returns a zerolog.Logger that discards all output.
func discardLogger() *zerolog.Logger {
	l := zerolog.New(io.Discard)
	return &l
}

// noAuthVatel returns a Vatel with no auth middleware attached.
func noAuthVatel(opts ...func(*Option)) *Vatel {
	return NewVatel(opts...)
}

// authVatel returns a Vatel with mock auth middleware and an auth.Config for
// building per-endpoint RequirePerms options.
func authVatel(allowed bool) (*Vatel, *mockDecoder, *mockAuthorizer, *testAuthConfig) {
	payload := &mockPayload{login: "alice", role: 1, perms: []byte{0xFF}}
	token := &mockTokener{payload: payload}
	dec := &mockDecoder{token: token}
	auth := &mockAuthorizer{allowed: allowed}

	v := NewVatel()

	cfg := authConfig(dec, auth)
	v.AddMiddleware(OnAuthentication, cfg.Middleware())

	return v, dec, auth, cfg
}

// authConfig builds a vatel/auth-style Config from mock dependencies.
// Imported inline here to avoid importing the subpackage from _test files.
type testAuthConfig struct {
	dec  *mockDecoder
	auth *mockAuthorizer
}

func authConfig(dec *mockDecoder, auth *mockAuthorizer) *testAuthConfig {
	return &testAuthConfig{dec: dec, auth: auth}
}

func (c *testAuthConfig) Middleware() func(Context, EndpointMeta) error {
	return func(ctx Context, _ EndpointMeta) error {
		at := ctx.Header("Authorization")
		if len(at) == 0 {
			return nil
		}
		token, err := c.dec.Decode(at)
		if err != nil {
			return ErrUnauthorized.Wrap(err)
		}
		ctx.SetTokenPayload(token.ApplicationPayload())
		return nil
	}
}

func (c *testAuthConfig) RequirePerms(perms map[string]uint) EndpointOption {
	bits := make([]uint, 0, len(perms))
	names := make([]string, 0, len(perms))
	for name, bit := range perms {
		names = append(names, name)
		bits = append(bits, bit)
	}
	return func(e *Endpoint) {
		e.AddMiddleware(OnAuthorization, func(ctx Context, _ EndpointMeta) error {
			tp := ctx.TokenPayload()
			if tp == nil {
				return ErrUnauthorized.New().Set("perms", names)
			}
			pa, ok := tp.(interface{ Perms() []byte })
			if !ok {
				return ErrUnauthorized.New()
			}
			allowed, err := c.auth.IsAllowed(pa.Perms(), bits...)
			if err != nil {
				return ErrUnauthorized.Wrap(err)
			}
			if !allowed {
				return ErrForbidden.New().Set("perms", names)
			}
			return nil
		})
	}
}

// compileAndRun compiles e against v, then invokes the resulting handler on fctx.
// Returns the handler function for callers that need it.
func compileAndRun(v *Vatel, e *Endpoint, fctx *fasthttp.RequestCtx) {
	if err := e.compile(v); err != nil {
		panic("compile: " + err.Error())
	}
	l := discardLogger()
	e.handler(l)(fctx)
}

// newGETCtx returns a fasthttp RequestCtx pre-configured for a GET request.
func newGETCtx(uri string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI(uri)
	return ctx
}

// newPOSTCtx returns a fasthttp RequestCtx pre-configured for a POST request with JSON body.
func newPOSTCtx(uri string, body []byte) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetRequestURI(uri)
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.SetBody(body)
	return ctx
}

// responseStatus reads the HTTP status from a test context (200 if not set).
func responseStatus(fctx *fasthttp.RequestCtx) int {
	return fctx.Response.StatusCode()
}
