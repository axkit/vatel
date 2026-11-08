package vatel

// This file demonstrates how to use vatel/auth-style middleware with
// project-specific JWT claims. The framework only requires that auth
// middleware calls ctx.SetTokenPayload(). Everything else (UserID type,
// field names, roles) is defined by the project.

import "testing"

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

type staticTokener struct{ payload AuthPayload }

func (t *staticTokener) SystemPayload() map[string]any   { return nil }
func (t *staticTokener) ApplicationPayload() AuthPayload { return t.payload }

type staticDecoder struct{ payload AuthPayload }

func (d *staticDecoder) Decode([]byte) (Tokener, error) {
	return &staticTokener{payload: d.payload}, nil
}

// ── Handlers ──────────────────────────────────────────────────────────────────

type intClaimsHandler struct {
	gotUserID int
	gotLogin  string
}

func (h *intClaimsHandler) Handle(ctx Context) error {
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

func (h *uuidClaimsHandler) Handle(ctx Context) error {
	claims := ctx.TokenPayload().(*uuidClaims)
	h.gotUserID = claims.UserID()
	h.gotEmail = claims.Email()
	h.gotRoles = claims.Roles()
	return nil
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestCustomClaims_IntUserID(t *testing.T) {
	claims := &intClaims{userID: 42, login: "alice", perms: []byte{0xFF}}
	dec := &staticDecoder{payload: claims}
	auth := &mockAuthorizer{allowed: true}
	cfg := authConfig((*mockDecoder)(nil), auth)
	cfg.dec = &mockDecoder{token: &staticTokener{payload: claims}}

	v := NewVatel()
	v.AddMiddleware(OnAuthentication, func(ctx Context, _ EndpointMeta) error {
		at := ctx.Header("Authorization")
		if len(at) == 0 {
			return nil
		}
		token, err := dec.Decode(at)
		if err != nil {
			return ErrUnauthorized.Wrap(err)
		}
		ctx.SetTokenPayload(token.ApplicationPayload())
		return nil
	})

	h := &intClaimsHandler{}
	fctx := newGETCtx("/profile")
	fctx.Request.Header.Set("Authorization", "Bearer test-token")

	run(v, GET("/profile", func() Handler { return h }, cfg.RequirePerms(map[string]uint{"profile:read": 0})), fctx)

	if responseStatus(fctx) != 200 {
		t.Fatalf("want 200, got %d", responseStatus(fctx))
	}
	if h.gotUserID != 42 {
		t.Errorf("userID: want 42, got %d", h.gotUserID)
	}
	if h.gotLogin != "alice" {
		t.Errorf("login: want alice, got %s", h.gotLogin)
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
	dec := &staticDecoder{payload: claims}
	auth := &mockAuthorizer{allowed: true}
	cfg := authConfig((*mockDecoder)(nil), auth)

	v := NewVatel()
	v.AddMiddleware(OnAuthentication, func(ctx Context, _ EndpointMeta) error {
		at := ctx.Header("Authorization")
		if len(at) == 0 {
			return nil
		}
		token, err := dec.Decode(at)
		if err != nil {
			return ErrUnauthorized.Wrap(err)
		}
		ctx.SetTokenPayload(token.ApplicationPayload())
		return nil
	})

	h := &uuidClaimsHandler{}
	fctx := newGETCtx("/content")
	fctx.Request.Header.Set("Authorization", "Bearer test-token")

	run(v, GET("/content", func() Handler { return h }, cfg.RequirePerms(map[string]uint{"content:write": 0})), fctx)

	if responseStatus(fctx) != 200 {
		t.Fatalf("want 200, got %d", responseStatus(fctx))
	}
	if h.gotUserID != uid {
		t.Errorf("userID: want %s, got %s", uid, h.gotUserID)
	}
	if h.gotEmail != "alice@example.com" {
		t.Errorf("email: want alice@example.com, got %s", h.gotEmail)
	}
	if len(h.gotRoles) != 2 || h.gotRoles[0] != "admin" {
		t.Errorf("roles: want [admin editor], got %v", h.gotRoles)
	}
}

// TestCustomClaims_NoAuth shows that the payload is available on public
// endpoints — the auth middleware sets it whenever a token is present.
func TestCustomClaims_NoAuth(t *testing.T) {
	claims := &intClaims{userID: 7, login: "bob", perms: []byte{}}
	dec := &staticDecoder{payload: claims}

	v := NewVatel()
	v.AddMiddleware(OnAuthentication, func(ctx Context, _ EndpointMeta) error {
		at := ctx.Header("Authorization")
		if len(at) == 0 {
			return nil
		}
		token, _ := dec.Decode(at)
		ctx.SetTokenPayload(token.ApplicationPayload())
		return nil
	})

	h := &intClaimsHandler{}
	fctx := newGETCtx("/public")
	fctx.Request.Header.Set("Authorization", "Bearer test-token")

	// No RequirePerms — public endpoint, no permission check.
	run(v, GET("/public", func() Handler { return h }), fctx)

	if responseStatus(fctx) != 200 {
		t.Fatalf("want 200, got %d", responseStatus(fctx))
	}
	if h.gotUserID != 7 {
		t.Errorf("want userID=7, got %d", h.gotUserID)
	}
}
