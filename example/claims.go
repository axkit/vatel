package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"os"
	"time"

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
	Exp       int64  `json:"exp,omitempty"` // expiry, Unix seconds; 0 = none
}

// Perms and Debug satisfy vatel.AuthPayload: vatel/auth uses Perms for
// RequirePerms and Debug to turn on verbose errors for the request.
func (c *Claims) Perms() []byte { return c.PermBytes }
func (c *Claims) Debug() bool   { return c.IsDebug }

// jwtTokener wraps decoded claims and satisfies vatel.Tokener.
type jwtTokener struct{ claims *Claims }

func (t *jwtTokener) SystemPayload() map[string]any         { return nil }
func (t *jwtTokener) ApplicationPayload() vatel.AuthPayload { return t.claims }

const (
	// jwtSecretEnv names the environment variable with the HS256 key.
	jwtSecretEnv = "JWT_SECRET"
	// devJWTSecret is the key used when JWT_SECRET is not set. It is public,
	// so anyone can sign tokens with it: set JWT_SECRET outside local
	// development.
	devJWTSecret = "vatel-example-dev-secret"
)

// jwtSecret returns the HS256 key from JWT_SECRET, or devJWTSecret with
// dev = true if it is not set.
func jwtSecret() (secret []byte, dev bool) {
	if s := os.Getenv(jwtSecretEnv); s != "" {
		return []byte(s), false
	}
	return []byte(devJWTSecret), true
}

var (
	errNoSecret     = stderrors.New("JWT secret is not set")
	errBadSignature = stderrors.New("invalid token signature")
	errTokenExpired = stderrors.New("token expired")
)

// jwtHeader is the only header SignToken writes and Decode accepts.
var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

// JWTDecoder decodes HS256 JWT tokens signed with Secret: the header must
// name HS256, the HMAC-SHA256 signature must match, and an exp claim, if
// set, must be in the future. A "Bearer " prefix is stripped. It uses only
// the standard library; a production service may prefer a JWT library such
// as github.com/golang-jwt/jwt/v5 (key rotation, other algorithms, nbf/aud).
type JWTDecoder struct {
	Secret []byte
}

func (d *JWTDecoder) Decode(token []byte) (vatel.Tokener, error) {
	if len(d.Secret) == 0 {
		return nil, errNoSecret
	}
	token = bytes.TrimPrefix(token, []byte("Bearer "))
	parts := bytes.Split(token, []byte("."))
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token: expected 3 parts, got %d", len(parts))
	}
	if err := checkHeader(parts[0]); err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(string(parts[2]))
	if err != nil {
		return nil, errBadSignature
	}
	signed := token[:len(parts[0])+1+len(parts[1])]
	if !hmac.Equal(sig, sign(d.Secret, signed)) {
		return nil, errBadSignature
	}

	// The payload is parsed only after the signature is verified.
	payload, err := base64.RawURLEncoding.DecodeString(string(parts[1]))
	if err != nil {
		return nil, fmt.Errorf("invalid token payload: %w", err)
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("malformed token claims: %w", err)
	}
	if claims.Exp != 0 && time.Now().Unix() >= claims.Exp {
		return nil, errTokenExpired
	}
	return &jwtTokener{claims: &claims}, nil
}

// checkHeader accepts only an HS256 header: a token cannot switch the
// algorithm (e.g. to "none").
func checkHeader(seg []byte) error {
	raw, err := base64.RawURLEncoding.DecodeString(string(seg))
	if err != nil {
		return fmt.Errorf("invalid token header: %w", err)
	}
	var h struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return fmt.Errorf("invalid token header: %w", err)
	}
	if h.Alg != "HS256" {
		return fmt.Errorf("unsupported token algorithm %q", h.Alg)
	}
	return nil
}

// sign returns the HMAC-SHA256 of data with secret.
func sign(secret, data []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(data)
	return m.Sum(nil)
}

// SignToken returns an HS256 JWT with claims c signed with secret. The
// example uses it to mint development tokens (go run ./example token).
func SignToken(secret []byte, c Claims) (string, error) {
	if len(secret) == 0 {
		return "", errNoSecret
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signed := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(payload)
	return signed + "." + base64.RawURLEncoding.EncodeToString(sign(secret, []byte(signed))), nil
}

// AllPerms returns the permission mask with every bit of Permissions set.
func AllPerms() []byte {
	var maxBit uint
	bits := make([]uint, 0, len(Permissions))
	for _, b := range Permissions {
		bits = append(bits, b)
		maxBit = max(maxBit, b)
	}
	bs := bitset.New(int(maxBit) + 1)
	bs.Set(true, bits...)
	return []byte(bs.String())
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
