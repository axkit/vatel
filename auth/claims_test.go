package auth

// This file demonstrates how to use the auth package with project-specific
// JWT claims. The framework only requires that auth middleware calls
// ctx.SetTokenPayload(). Everything else (UserID type, field names, roles) is
// defined by the project; Config.Middleware stores whatever payload the
// project's Decoder returns, and handlers type-assert it to the concrete type.

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/axkit/vatel"
)

// ── Project A: integer user IDs ───────────────────────────────────────────────

type intClaims struct {
	userID int
	login  string
	perms  []byte
	debug  bool
}

func (c *intClaims) Perms() []byte { return c.perms }
func (c *intClaims) Debug() bool   { return c.debug }
func (c *intClaims) UserID() int   { return c.userID }
func (c *intClaims) Login() string { return c.login }

// ── Project B: UUID user IDs ──────────────────────────────────────────────────

type uuidClaims struct {
	userID string // e.g. "550e8400-e29b-41d4-a716-446655440000"
	email  string
	roles  []string
	perms  []byte
}

func (c *uuidClaims) Perms() []byte   { return c.perms }
func (c *uuidClaims) Debug() bool     { return false }
func (c *uuidClaims) UserID() string  { return c.userID }
func (c *uuidClaims) Email() string   { return c.email }
func (c *uuidClaims) Roles() []string { return c.roles }

// ── Shared token decoder helpers ──────────────────────────────────────────────

type claimsTokener struct{ payload vatel.AuthPayload }

func (t *claimsTokener) SystemPayload() map[string]any         { return nil }
func (t *claimsTokener) ApplicationPayload() vatel.AuthPayload { return t.payload }

const claimsToken = "test-token"

// claimsDecoder stands in for a real JWT decoder: it returns payload for
// claimsToken. Config.Decoder receives the raw Authorization header value,
// so the decoder strips the "Bearer " prefix itself.
type claimsDecoder struct{ payload vatel.AuthPayload }

func (d *claimsDecoder) Decode(at []byte) (vatel.Tokener, error) {
	tok, ok := bytes.CutPrefix(at, []byte("Bearer "))
	if !ok || string(tok) != claimsToken {
		return nil, errors.New("invalid token")
	}
	return &claimsTokener{payload: d.payload}, nil
}

// newClaimsServer serves h at path behind cfg.Middleware, adding opts to the
// endpoint.
func newClaimsServer(t *testing.T, cfg *Config, path string, h vatel.Handler, opts ...vatel.EndpointOption) func(token string) int {
	t.Helper()
	v := vatel.NewVatel()
	v.AddMiddleware(vatel.OnAuthentication, cfg.Middleware())
	v.Add(vatel.Endpoints{vatel.GET(path, func() vatel.Handler { return h }, opts...)})
	srv := build(t, v)
	return func(token string) int {
		return get(srv, path, token).Response.StatusCode()
	}
}

// ── Handlers ──────────────────────────────────────────────────────────────────

type intClaimsHandler struct {
	gotUserID int
	gotLogin  string
}

func (h *intClaimsHandler) Handle(ctx vatel.Context) error {
	claims := ctx.TokenPayload().(*intClaims)
	h.gotUserID = claims.UserID()
	h.gotLogin = claims.Login()
	return nil
}

type uuidClaimsHandler struct {
	gotUserID string
	gotEmail  string
	gotRoles  []string
}

func (h *uuidClaimsHandler) Handle(ctx vatel.Context) error {
	claims := ctx.TokenPayload().(*uuidClaims)
	h.gotUserID = claims.UserID()
	h.gotEmail = claims.Email()
	h.gotRoles = claims.Roles()
	return nil
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestCustomClaims_IntUserID(t *testing.T) {
	claims := &intClaims{userID: 42, login: "alice", perms: []byte{0xFF}}
	cfg := &Config{Decoder: &claimsDecoder{payload: claims}, Authorizer: allowAll{}}

	h := &intClaimsHandler{}
	do := newClaimsServer(t, cfg, "/profile", h, cfg.RequirePerms(map[string]uint{"profile:read": 0}))

	if st := do("Bearer " + claimsToken); st != 200 {
		t.Fatalf("want 200, got %d", st)
	}
	if h.gotUserID != 42 {
		t.Errorf("userID: want 42, got %d", h.gotUserID)
	}
	if h.gotLogin != "alice" {
		t.Errorf("login: want alice, got %s", h.gotLogin)
	}

	if st := do(""); st != 401 {
		t.Errorf("no token: want 401, got %d", st)
	}
	if st := do(claimsToken); st != 401 {
		t.Errorf("token without Bearer prefix: want 401, got %d", st)
	}
}

func TestCustomClaims_UUIDUserID(t *testing.T) {
	const uid = "550e8400-e29b-41d4-a716-446655440000"
	claims := &uuidClaims{
		userID: uid,
		email:  "alice@example.com",
		roles:  []string{"admin", "editor"},
		perms:  []byte{0xFF},
	}
	cfg := &Config{Decoder: &claimsDecoder{payload: claims}, Authorizer: allowAll{}}

	h := &uuidClaimsHandler{}
	do := newClaimsServer(t, cfg, "/content", h, cfg.RequirePerms(map[string]uint{"content:write": 0}))

	if st := do("Bearer " + claimsToken); st != 200 {
		t.Fatalf("want 200, got %d", st)
	}
	if h.gotUserID != uid {
		t.Errorf("userID: want %s, got %s", uid, h.gotUserID)
	}
	if h.gotEmail != "alice@example.com" {
		t.Errorf("email: want alice@example.com, got %s", h.gotEmail)
	}
	if want := []string{"admin", "editor"}; !reflect.DeepEqual(h.gotRoles, want) {
		t.Errorf("roles: want %v, got %v", want, h.gotRoles)
	}
}

// TestCustomClaims_NoAuth shows that the payload is available on public
// endpoints — the auth middleware sets it whenever a valid token is present.
func TestCustomClaims_NoAuth(t *testing.T) {
	claims := &intClaims{userID: 7, login: "bob", perms: []byte{}}
	cfg := &Config{Decoder: &claimsDecoder{payload: claims}}

	h := &intClaimsHandler{}
	// No RequirePerms — public endpoint, no permission check.
	do := newClaimsServer(t, cfg, "/public", h)

	if st := do("Bearer " + claimsToken); st != 200 {
		t.Fatalf("want 200, got %d", st)
	}
	if h.gotUserID != 7 {
		t.Errorf("want userID=7, got %d", h.gotUserID)
	}
}
