package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/axkit/bitset"
	"github.com/axkit/vatel"
	"github.com/axkit/vatel/auth"
)

// Claims is the application's JWT payload. Handlers receive it via
// ctx.TokenPayload().(*Claims) after the auth middleware runs.
type Claims struct {
	UserID    int    `json:"sub"`
	Login     string `json:"login"`
	Role      int    `json:"role"`
	PermBytes []byte `json:"perms"` // bitset-encoded permission mask
	IsDebug   bool   `json:"debug"`
}

// Perms and Debug satisfy vatel.AuthPayload: vatel/auth uses Perms for
// RequirePerms and Debug to turn on verbose errors for the request.
func (c *Claims) Perms() []byte { return c.PermBytes }
func (c *Claims) Debug() bool   { return c.IsDebug }

// jwtTokener wraps decoded claims and satisfies vatel.Tokener.
type jwtTokener struct{ claims *Claims }

func (t *jwtTokener) SystemPayload() map[string]any         { return nil }
func (t *jwtTokener) ApplicationPayload() vatel.AuthPayload { return t.claims }

// JWTDecoder decodes JWT tokens by base64-decoding the payload segment.
//
// WARNING: this example skips signature verification for brevity, so it
// needs no secret. In production use a library such as
// github.com/golang-jwt/jwt/v5 and verify the signature with your key.
type JWTDecoder struct{}

func (d *JWTDecoder) Decode(token []byte) (vatel.Tokener, error) {
	parts := bytes.Split(token, []byte("."))
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token: expected 3 parts, got %d", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(string(parts[1]))
	if err != nil {
		return nil, fmt.Errorf("invalid token payload: %w", err)
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("malformed token claims: %w", err)
	}
	return &jwtTokener{claims: &claims}, nil
}

// BitsetAuthorizer checks whether the token's permission bytes satisfy the
// required bit positions using github.com/axkit/bitset.
// All required bits must be set (bitset.All rule).
type BitsetAuthorizer struct{}

func (a *BitsetAuthorizer) IsAllowed(requestPerms []byte, requiredBits ...uint) (bool, error) {
	return bitset.AreSet(requestPerms, bitset.All, requiredBits...)
}

// Compile-time check: BitsetAuthorizer must satisfy auth.Authorizer.
var _ auth.Authorizer = (*BitsetAuthorizer)(nil)

// Permissions maps each permission name to its bit position in the token's
// permission bytes. Pass these to cfg.RequirePerms at endpoint registration.
var Permissions = map[string]uint{
	"orders:read":  0,
	"orders:write": 1,
	"users:read":   2,
	"import":       3,
}
