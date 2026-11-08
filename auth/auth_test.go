package auth

import (
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/axkit/vatel"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

type payload struct{ perms []byte }

func (p *payload) Perms() []byte { return p.perms }
func (p *payload) Debug() bool   { return false }

type tokener struct{ p vatel.AuthPayload }

func (t *tokener) SystemPayload() map[string]any         { return nil }
func (t *tokener) ApplicationPayload() vatel.AuthPayload { return t.p }

// decoder accepts the token "good" and rejects anything else as expired.
type decoder struct{}

func (decoder) Decode(tok []byte) (vatel.Tokener, error) {
	if string(tok) == "good" {
		return &tokener{p: &payload{perms: []byte{0xFF}}}, nil
	}
	return nil, errors.New("token expired")
}

type allowAll struct{}

func (allowAll) IsAllowed([]byte, ...uint) (bool, error) { return true, nil }

type revoker struct{}

func (revoker) IsTokenRevoked(tok string) (bool, error) { return tok == "revoked", nil }

type whoami struct {
	vatel.Output[struct {
		Authenticated bool `json:"authenticated"`
	}]
}

func (h *whoami) Handle(ctx vatel.Context) error {
	h.Out.Authenticated = ctx.TokenPayload() != nil
	return nil
}

func newServer(t *testing.T) fasthttp.RequestHandler {
	t.Helper()
	cfg := &Config{Decoder: decoder{}, Authorizer: allowAll{}, RevokeCheck: revoker{}}

	v := vatel.NewVatel()
	v.AddMiddleware(vatel.OnAuthentication, cfg.Middleware())
	ctrl := func() vatel.Handler { return &whoami{} }
	v.Add(vatel.Endpoints{
		vatel.GET("/public", ctrl),
		vatel.GET("/auth", ctrl, cfg.RequireAuth()),
		vatel.GET("/perms", ctrl, cfg.RequirePerms(map[string]uint{"read": 0})),
	})

	mux := router.New()
	l := zerolog.New(io.Discard)
	if err := v.BuildHandlers(mux, &l); err != nil {
		t.Fatal(err)
	}
	return mux.Handler
}

func get(h fasthttp.RequestHandler, path, token string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI(path)
	if token != "" {
		ctx.Request.Header.Set("Authorization", token)
	}
	h(ctx)
	return ctx
}

func TestBadTokenOnPublicEndpoint(t *testing.T) {
	h := newServer(t)
	for _, tok := range []string{"expired", "revoked"} {
		ctx := get(h, "/public", tok)
		if ctx.Response.StatusCode() != 200 {
			t.Errorf("token %q: want 200 on public endpoint, got %d: %s", tok, ctx.Response.StatusCode(), ctx.Response.Body())
		}
		var out struct{ Authenticated bool }
		_ = json.Unmarshal(ctx.Response.Body(), &out)
		if out.Authenticated {
			t.Errorf("token %q: request must be served as unauthenticated", tok)
		}
	}
}

func TestBadTokenOnProtectedEndpoint(t *testing.T) {
	h := newServer(t)
	tests := []struct {
		path, token string
		wantMsg     string
	}{
		{"/auth", "expired", "unauthorized"},
		{"/perms", "expired", "unauthorized"},
		{"/auth", "revoked", "access token revoked"},
		{"/perms", "revoked", "access token revoked"},
		{"/auth", "", "authentication required"},
		{"/perms", "", "authentication required"},
	}
	for _, tt := range tests {
		ctx := get(h, tt.path, tt.token)
		if ctx.Response.StatusCode() != 401 {
			t.Errorf("%s token %q: want 401, got %d", tt.path, tt.token, ctx.Response.StatusCode())
		}
		var out struct{ Msg string }
		_ = json.Unmarshal(ctx.Response.Body(), &out)
		if out.Msg != tt.wantMsg {
			t.Errorf("%s token %q: want msg %q, got %s", tt.path, tt.token, tt.wantMsg, ctx.Response.Body())
		}
	}
}

func TestGoodToken(t *testing.T) {
	h := newServer(t)
	for _, p := range []string{"/public", "/auth", "/perms"} {
		ctx := get(h, p, "good")
		if ctx.Response.StatusCode() != 200 {
			t.Errorf("%s: want 200, got %d: %s", p, ctx.Response.StatusCode(), ctx.Response.Body())
		}
		var out struct{ Authenticated bool }
		_ = json.Unmarshal(ctx.Response.Body(), &out)
		if !out.Authenticated {
			t.Errorf("%s: want authenticated", p)
		}
	}
}

func build(t *testing.T, v *vatel.Vatel) fasthttp.RequestHandler {
	t.Helper()
	mux := router.New()
	l := zerolog.New(io.Discard)
	if err := v.BuildHandlers(mux, &l); err != nil {
		t.Fatal(err)
	}
	return mux.Handler
}

func TestRequirePermsSetsMetaPerms(t *testing.T) {
	cfg := &Config{Decoder: decoder{}, Authorizer: allowAll{}}

	var got []string
	v := vatel.NewVatel()
	v.AddMiddleware(vatel.OnAuthentication, cfg.Middleware())
	v.AddMiddleware(vatel.OnHandle, func(_ vatel.Context, m vatel.EndpointMeta) error {
		got = m.Perms
		return nil
	})
	v.Add(vatel.Endpoints{
		vatel.GET("/perms", func() vatel.Handler { return &whoami{} },
			cfg.RequirePerms(map[string]uint{"orders:write": 1, "orders:read": 0, "admin": 2})),
	})
	h := build(t, v)

	ctx := get(h, "/perms", "good")
	if ctx.Response.StatusCode() != 200 {
		t.Fatalf("want 200, got %d: %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	want := []string{"admin", "orders:read", "orders:write"}
	if len(got) != len(want) {
		t.Fatalf("want perms %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want perms %v, got %v", want, got)
		}
	}
}

func TestDisabledAfterOptionsCreated(t *testing.T) {
	cfg := &Config{Decoder: decoder{}, Authorizer: allowAll{}}

	v := vatel.NewVatel()
	v.AddMiddleware(vatel.OnAuthentication, cfg.Middleware())
	ctrl := func() vatel.Handler { return &whoami{} }
	v.Add(vatel.Endpoints{
		vatel.GET("/auth", ctrl, cfg.RequireAuth()),
		vatel.GET("/perms", ctrl, cfg.RequirePerms(map[string]uint{"read": 0})),
	})
	h := build(t, v)

	for _, p := range []string{"/auth", "/perms"} {
		if ctx := get(h, p, ""); ctx.Response.StatusCode() != 401 {
			t.Fatalf("%s enabled: want 401, got %d", p, ctx.Response.StatusCode())
		}
	}

	cfg.Disabled = true
	for _, p := range []string{"/auth", "/perms"} {
		if ctx := get(h, p, ""); ctx.Response.StatusCode() != 200 {
			t.Errorf("%s disabled: want 200, got %d: %s", p, ctx.Response.StatusCode(), ctx.Response.Body())
		}
	}
}

func TestNilAuthorizer(t *testing.T) {
	cfg := &Config{Decoder: decoder{}}

	v := vatel.NewVatel()
	v.AddMiddleware(vatel.OnAuthentication, cfg.Middleware())
	v.Add(vatel.Endpoints{
		vatel.GET("/perms", func() vatel.Handler { return &whoami{} }, cfg.RequirePerms(map[string]uint{"read": 0})),
	})
	h := build(t, v)

	ctx := get(h, "/perms", "good")
	if ctx.Response.StatusCode() != 500 {
		t.Errorf("want 500, got %d: %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
}
