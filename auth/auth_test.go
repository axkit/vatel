package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/axkit/vatel"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

type payload struct {
	perms []byte
	debug bool
}

func (p *payload) Perms() []byte { return p.perms }
func (p *payload) Debug() bool   { return p.debug }

type tokener struct{ p vatel.AuthPayload }

func (t *tokener) SystemPayload() map[string]any         { return nil }
func (t *tokener) ApplicationPayload() vatel.AuthPayload { return t.p }

// decoder accepts the tokens "good" and "debug" (a payload with Debug() true)
// and rejects anything else as expired.
type decoder struct{}

func (decoder) Decode(tok []byte) (vatel.Tokener, error) {
	switch string(tok) {
	case "good":
		return &tokener{p: &payload{perms: []byte{0xFF}}}, nil
	case "debug":
		return &tokener{p: &payload{perms: []byte{0xFF}, debug: true}}, nil
	}
	return nil, errors.New("token expired")
}

type allowAll struct{}

func (allowAll) IsAllowed([]byte, ...uint) (bool, error) { return true, nil }

// denyAll denies every request.
type denyAll struct{}

func (denyAll) IsAllowed([]byte, ...uint) (bool, error) { return false, nil }

// failingAuthorizer fails every permission check.
type failingAuthorizer struct{}

func (failingAuthorizer) IsAllowed([]byte, ...uint) (bool, error) {
	return false, errors.New("bitset: corrupted")
}

// recordingAuthorizer allows every request and records its arguments.
type recordingAuthorizer struct {
	requestPerms  []byte
	endpointPerms []uint
}

func (r *recordingAuthorizer) IsAllowed(requestPerms []byte, endpointPerms ...uint) (bool, error) {
	r.requestPerms = requestPerms
	r.endpointPerms = endpointPerms
	return true, nil
}

// revoker reports the token "revoked" as revoked and fails the check for the
// token "revoke-error".
type revoker struct{}

const revokeErrText = "revocation store unavailable"

func (revoker) IsTokenRevoked(tok string) (bool, error) {
	if tok == "revoke-error" {
		return false, errors.New(revokeErrText)
	}
	return tok == "revoked", nil
}

type whoamiOut struct {
	Authenticated bool   `json:"authenticated"`
	Perms         []byte `json:"perms"`
}

type whoami struct {
	vatel.Output[whoamiOut]
}

func (h *whoami) Handle(ctx vatel.Context) error {
	tp := ctx.TokenPayload()
	h.Out.Authenticated = tp != nil
	if p, ok := tp.(*payload); ok {
		h.Out.Perms = p.Perms()
	}
	return nil
}

// errBody is the JSON shape of a vatel error response.
type errBody struct {
	Msg    string         `json:"msg"`
	Code   string         `json:"code"`
	Fields map[string]any `json:"fields"`
}

func newServer(t *testing.T) fasthttp.RequestHandler {
	t.Helper()
	return newServerWith(t, &Config{Decoder: decoder{}, Authorizer: allowAll{}, RevokeCheck: revoker{}})
}

// newServerWith serves /public, /auth (RequireAuth) and /perms (RequirePerms)
// with whoami, using cfg.Middleware for authentication.
func newServerWith(t *testing.T, cfg *Config) fasthttp.RequestHandler {
	t.Helper()
	v := vatel.NewVatel()
	v.AddMiddleware(vatel.OnAuthentication, cfg.Middleware())
	ctrl := func() vatel.Handler { return &whoami{} }
	v.Add(vatel.Endpoints{
		vatel.GET("/public", ctrl),
		vatel.GET("/auth", ctrl, cfg.RequireAuth()),
		vatel.GET("/perms", ctrl, cfg.RequirePerms(map[string]uint{"read": 0})),
	})
	return build(t, v)
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

// decodeBody unmarshals the response body into v, failing the test if the
// body is not valid JSON.
func decodeBody(t *testing.T, ctx *fasthttp.RequestCtx, v any) {
	t.Helper()
	if err := json.Unmarshal(ctx.Response.Body(), v); err != nil {
		t.Fatalf("invalid JSON body %q: %v", ctx.Response.Body(), err)
	}
}

// wantAuthenticated asserts a 200 whoami response with the given
// authentication state.
func wantAuthenticated(t *testing.T, ctx *fasthttp.RequestCtx, label string, want bool) {
	t.Helper()
	if ctx.Response.StatusCode() != 200 {
		t.Errorf("%s: want 200, got %d: %s", label, ctx.Response.StatusCode(), ctx.Response.Body())
		return
	}
	var out whoamiOut
	decodeBody(t, ctx, &out)
	if out.Authenticated != want {
		t.Errorf("%s: want authenticated=%v, got %s", label, want, ctx.Response.Body())
	}
}

// wantError asserts an error response with the given status and vatel code.
func wantError(t *testing.T, ctx *fasthttp.RequestCtx, label string, status int, code string) errBody {
	t.Helper()
	if ctx.Response.StatusCode() != status {
		t.Errorf("%s: want %d, got %d: %s", label, status, ctx.Response.StatusCode(), ctx.Response.Body())
	}
	var out errBody
	decodeBody(t, ctx, &out)
	if out.Code != code {
		t.Errorf("%s: want code %s, got %s", label, code, ctx.Response.Body())
	}
	return out
}

func TestBadTokenOnPublicEndpoint(t *testing.T) {
	h := newServer(t)
	for _, tok := range []string{"expired", "revoked"} {
		wantAuthenticated(t, get(h, "/public", tok), "token "+tok, false)
	}
}

func TestBadTokenOnProtectedEndpoint(t *testing.T) {
	h := newServer(t)
	tests := []struct {
		path, token string
		wantCode    string
		wantMsg     string
	}{
		{"/auth", "expired", "VTL-0004", "unauthorized"},
		{"/perms", "expired", "VTL-0004", "unauthorized"},
		{"/auth", "revoked", "VTL-0002", "access token revoked"},
		{"/perms", "revoked", "VTL-0002", "access token revoked"},
		{"/auth", "", "VTL-0004", "authentication required"},
		{"/perms", "", "VTL-0004", "authentication required"},
	}
	for _, tt := range tests {
		label := tt.path + " token " + tt.token
		out := wantError(t, get(h, tt.path, tt.token), label, 401, tt.wantCode)
		if out.Msg != tt.wantMsg {
			t.Errorf("%s: want msg %q, got %q", label, tt.wantMsg, out.Msg)
		}
	}
}

func TestGoodToken(t *testing.T) {
	h := newServer(t)
	for _, p := range []string{"/public", "/auth", "/perms"} {
		ctx := get(h, p, "good")
		wantAuthenticated(t, ctx, p, true)
		var out whoamiOut
		decodeBody(t, ctx, &out)
		if !bytes.Equal(out.Perms, []byte{0xFF}) {
			t.Errorf("%s: handler must see the decoded payload, got perms %v", p, out.Perms)
		}
	}
}

func TestRevokeCheckError(t *testing.T) {
	h := newServer(t)

	wantAuthenticated(t, get(h, "/public", "revoke-error"), "/public", false)

	for _, p := range []string{"/auth", "/perms"} {
		ctx := get(h, p, "revoke-error")
		wantError(t, ctx, p, 500, "VTL-0005")
		if bytes.Contains(ctx.Response.Body(), []byte(revokeErrText)) {
			t.Errorf("%s: RevokeCheck error text must not reach the client: %s", p, ctx.Response.Body())
		}
	}
}

func TestNilDecoder(t *testing.T) {
	h := newServerWith(t, &Config{Authorizer: allowAll{}})

	wantAuthenticated(t, get(h, "/public", "good"), "/public", false)
	wantError(t, get(h, "/auth", "good"), "/auth", 500, "VTL-0005")
	wantError(t, get(h, "/perms", "good"), "/perms", 500, "VTL-0005")
	// Without a token the Decoder is not needed.
	wantError(t, get(h, "/auth", ""), "/auth no token", 401, "VTL-0004")
}

func TestRequirePermsForbidden(t *testing.T) {
	h := newServerWith(t, &Config{Decoder: decoder{}, Authorizer: denyAll{}})

	wantError(t, get(h, "/perms", "good"), "/perms", 403, "VTL-0003")
	// RequireAuth does not consult the Authorizer.
	wantAuthenticated(t, get(h, "/auth", "good"), "/auth", true)
}

func TestRequirePermsAuthorizerError(t *testing.T) {
	h := newServerWith(t, &Config{Decoder: decoder{}, Authorizer: failingAuthorizer{}})

	wantError(t, get(h, "/perms", "good"), "/perms", 401, "VTL-0004")
}

// TestRequirePermsPayloadWithoutPerms covers a custom authentication
// middleware that sets a payload without a Perms method.
func TestRequirePermsPayloadWithoutPerms(t *testing.T) {
	cfg := &Config{Authorizer: allowAll{}}

	v := vatel.NewVatel()
	v.AddMiddleware(vatel.OnAuthentication, func(ctx vatel.Context, _ vatel.EndpointMeta) error {
		ctx.SetTokenPayload(struct{ UserID int }{UserID: 1})
		return nil
	})
	ctrl := func() vatel.Handler { return &whoami{} }
	v.Add(vatel.Endpoints{
		vatel.GET("/auth", ctrl, cfg.RequireAuth()),
		vatel.GET("/perms", ctrl, cfg.RequirePerms(map[string]uint{"read": 0})),
	})
	h := build(t, v)

	wantError(t, get(h, "/perms", ""), "/perms", 401, "VTL-0004")
	wantAuthenticated(t, get(h, "/auth", ""), "/auth", true)
}

type validationFailure struct{}

func (validationFailure) Handle(vatel.Context) error {
	return vatel.ErrValidationFailed.New().Set("field", "x")
}

func TestDebugPayloadEnablesVerboseErrors(t *testing.T) {
	cfg := &Config{Decoder: decoder{}, Authorizer: allowAll{}}

	v := vatel.NewVatel()
	v.AddMiddleware(vatel.OnAuthentication, cfg.Middleware())
	v.Add(vatel.Endpoints{
		vatel.GET("/fail", func() vatel.Handler { return validationFailure{} }),
	})
	h := build(t, v)

	out := wantError(t, get(h, "/fail", "debug"), "debug token", 400, "VTL-0006")
	if out.Fields["field"] != "x" {
		t.Errorf("debug token: want verbose error with fields, got %v", out.Fields)
	}

	for _, tok := range []string{"good", ""} {
		ctx := get(h, "/fail", tok)
		out := wantError(t, ctx, "token "+tok, 400, "VTL-0006")
		if out.Fields != nil || bytes.Contains(ctx.Response.Body(), []byte(`"stack"`)) {
			t.Errorf("token %q: want non-verbose error, got %s", tok, ctx.Response.Body())
		}
	}
}

func TestRequirePermsBitOrder(t *testing.T) {
	az := &recordingAuthorizer{}
	cfg := &Config{Decoder: decoder{}, Authorizer: az}

	var metaPerms []string
	v := vatel.NewVatel()
	v.AddMiddleware(vatel.OnAuthentication, cfg.Middleware())
	v.AddMiddleware(vatel.OnHandle, func(_ vatel.Context, m vatel.EndpointMeta) error {
		metaPerms = m.Perms
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
	// Names are sorted (admin, orders:read, orders:write); bits follow them.
	if want := []string{"admin", "orders:read", "orders:write"}; !reflect.DeepEqual(metaPerms, want) {
		t.Errorf("meta perms: want %v, got %v", want, metaPerms)
	}
	if want := []uint{2, 0, 1}; !reflect.DeepEqual(az.endpointPerms, want) {
		t.Errorf("endpoint perms: want %v, got %v", want, az.endpointPerms)
	}
	if want := []byte{0xFF}; !bytes.Equal(az.requestPerms, want) {
		t.Errorf("request perms: want %v, got %v", want, az.requestPerms)
	}
}

// TestDisabledAfterOptionsCreated sets Disabled after the options are created
// but while no request is in flight (it is not synchronised).
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
		wantAuthenticated(t, get(h, p, ""), p+" disabled", false)
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

	wantError(t, get(h, "/perms", "good"), "/perms", 500, "VTL-0005")
}
