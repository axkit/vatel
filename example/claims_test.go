package main

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/axkit/vatel"
	"github.com/axkit/vatel/auth"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

var testSecret = []byte("test-secret")

func mustSign(t *testing.T, secret []byte, c Claims) string {
	t.Helper()
	tok, err := SignToken(secret, c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestJWTDecoder_ValidToken(t *testing.T) {
	d := &JWTDecoder{Secret: testSecret}
	tok := mustSign(t, testSecret, Claims{UserID: 100, Login: "alice", PermBytes: AllPerms()})
	for _, raw := range []string{tok, "Bearer " + tok} {
		tk, err := d.Decode([]byte(raw))
		if err != nil {
			t.Fatalf("Decode(%q) = %v", raw, err)
		}
		c := tk.ApplicationPayload().(*Claims)
		if c.UserID != 100 || c.Login != "alice" {
			t.Errorf("claims = %+v", c)
		}
	}
}

// A token whose payload was changed, signed with another key, with alg
// "none" or without a signature must be rejected: otherwise a client could
// forge claims and permissions.
func TestJWTDecoder_RejectsForgedTokens(t *testing.T) {
	d := &JWTDecoder{Secret: testSecret}
	good := mustSign(t, testSecret, Claims{UserID: 101, Login: "bob"})
	parts := strings.Split(good, ".")

	forgedPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":1,"login":"admin","perms":"` +
		base64.StdEncoding.EncodeToString(AllPerms()) + `"}`))
	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))

	tests := map[string]string{
		"tampered payload": parts[0] + "." + forgedPayload + "." + parts[2],
		"other key":        mustSign(t, []byte("other-secret"), Claims{UserID: 1, PermBytes: AllPerms()}),
		"alg none":         noneHeader + "." + forgedPayload + ".",
		"no signature":     parts[0] + "." + parts[1] + ".",
		"two parts":        parts[0] + "." + parts[1],
		"bad base64 sig":   parts[0] + "." + parts[1] + ".!!!",
		"expired":          mustSign(t, testSecret, Claims{UserID: 1, Exp: time.Now().Add(-time.Minute).Unix()}),
	}
	for name, tok := range tests {
		t.Run(name, func(t *testing.T) {
			if tk, err := d.Decode([]byte(tok)); err == nil {
				t.Errorf("Decode = %+v, want an error", tk.ApplicationPayload())
			}
		})
	}
}

func TestJWTDecoder_NoSecret(t *testing.T) {
	tok := mustSign(t, testSecret, Claims{UserID: 1})
	if _, err := (&JWTDecoder{}).Decode([]byte(tok)); err == nil {
		t.Error("Decode without a secret = nil, want an error")
	}
	if _, err := SignToken(nil, Claims{}); err == nil {
		t.Error("SignToken without a secret = nil, want an error")
	}
}

func TestJWTSecret(t *testing.T) {
	t.Setenv(jwtSecretEnv, "")
	s, dev := jwtSecret()
	if !dev || !bytes.Equal(s, []byte(devJWTSecret)) {
		t.Errorf("jwtSecret() = %q, %v; want the dev secret", s, dev)
	}
	t.Setenv(jwtSecretEnv, "prod")
	s, dev = jwtSecret()
	if dev || string(s) != "prod" {
		t.Errorf("jwtSecret() = %q, %v; want prod", s, dev)
	}
}

func TestAllPerms(t *testing.T) {
	a := &BitsetAuthorizer{}
	for name, bit := range Permissions {
		ok, err := a.IsAllowed(AllPerms(), bit)
		if err != nil || !ok {
			t.Errorf("%s: IsAllowed = %v, %v; want true", name, ok, err)
		}
	}
}

// End to end through vatel/auth: a signed token passes RequirePerms, a token
// with forged permissions is treated as no token.
func TestAuthFlow_SignedVsForged(t *testing.T) {
	authCfg := &auth.Config{Decoder: &JWTDecoder{Secret: testSecret}, Authorizer: &BitsetAuthorizer{}}
	v := vatel.NewVatel()
	v.AddMiddleware(vatel.OnAuthentication, authCfg.Middleware())
	v.Add(OrderEndpoints(authCfg))
	mux := router.New()
	l := zerolog.Nop()
	if err := v.BuildHandlers(mux, &l); err != nil {
		t.Fatal(err)
	}

	get := func(token string) int {
		var fctx fasthttp.RequestCtx
		fctx.Request.Header.SetMethod(fasthttp.MethodGet)
		fctx.Request.SetRequestURI("/orders")
		if token != "" {
			fctx.Request.Header.Set("Authorization", "Bearer "+token)
		}
		mux.Handler(&fctx)
		return fctx.Response.StatusCode()
	}

	good := mustSign(t, testSecret, Claims{UserID: 100, PermBytes: AllPerms()})
	if got := get(good); got != fasthttp.StatusOK {
		t.Errorf("signed token: status = %d, want 200", got)
	}
	noPerms := mustSign(t, testSecret, Claims{UserID: 100})
	if got := get(noPerms); got != fasthttp.StatusForbidden {
		t.Errorf("token without perms: status = %d, want 403", got)
	}
	parts := strings.Split(noPerms, ".")
	forged := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":100,"perms":"` +
		base64.StdEncoding.EncodeToString(AllPerms()) + `"}`))
	if got := get(parts[0] + "." + forged + "." + parts[2]); got != fasthttp.StatusUnauthorized {
		t.Errorf("forged token: status = %d, want 401", got)
	}
}

func TestPrintDevToken(t *testing.T) {
	if err := printDevToken(nil); err == nil {
		t.Error("printDevToken without a secret = nil, want an error")
	}
}
