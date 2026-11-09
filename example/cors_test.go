package main

import (
	"testing"

	"github.com/axkit/vatel"
	"github.com/axkit/vatel/auth"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

func TestCORSPreflight(t *testing.T) {
	v := vatel.NewVatel()
	v.Add(HealthEndpoints())
	mux := router.New()
	mux.GlobalOPTIONS = corsPreflight
	l := zerolog.Nop()
	if err := v.BuildHandlers(mux, &l); err != nil {
		t.Fatal(err)
	}

	preflight := func(path string) *fasthttp.RequestCtx {
		var fctx fasthttp.RequestCtx
		fctx.Request.Header.SetMethod(fasthttp.MethodOptions)
		fctx.Request.SetRequestURI(path)
		fctx.Request.Header.Set("Origin", "http://localhost:3000")
		mux.Handler(&fctx)
		return &fctx
	}

	fctx := preflight("/healthcheck")
	if got := fctx.Response.StatusCode(); got != fasthttp.StatusNoContent {
		t.Fatalf("status = %d, want 204", got)
	}
	h := &fctx.Response.Header
	if got := string(h.Peek("Access-Control-Allow-Origin")); got != "http://localhost:3000" {
		t.Errorf("Access-Control-Allow-Origin = %q", got)
	}
	if got := string(h.Peek("Access-Control-Allow-Methods")); got != "GET, OPTIONS" {
		t.Errorf("Access-Control-Allow-Methods = %q", got)
	}
	if got := string(h.Peek("Access-Control-Allow-Headers")); got != corsAllowHeaders {
		t.Errorf("Access-Control-Allow-Headers = %q", got)
	}

	if got := preflight("/unknown").Response.StatusCode(); got != fasthttp.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404", got)
	}
}

func TestCORSMiddleware(t *testing.T) {
	authCfg := &auth.Config{Decoder: &JWTDecoder{secret: "s"}, Authorizer: &BitsetAuthorizer{}}
	v := vatel.NewVatel()
	v.AddMiddleware(vatel.OnRequest, corsMiddleware)
	v.AddMiddleware(vatel.OnAuthentication, authCfg.Middleware())
	v.Add(HealthEndpoints(), vatel.Endpoints{
		vatel.GET("/private", vatel.Func(func(vatel.Context) error { return nil }), authCfg.RequireAuth()),
	})
	mux := router.New()
	l := zerolog.Nop()
	if err := v.BuildHandlers(mux, &l); err != nil {
		t.Fatal(err)
	}

	for path, want := range map[string]int{"/healthcheck": 200, "/private": 401} {
		var fctx fasthttp.RequestCtx
		fctx.Request.Header.SetMethod(fasthttp.MethodGet)
		fctx.Request.SetRequestURI(path)
		fctx.Request.Header.Set("Origin", "http://localhost:3000")
		mux.Handler(&fctx)
		if got := fctx.Response.StatusCode(); got != want {
			t.Errorf("%s: status = %d, want %d", path, got, want)
		}
		if got := string(fctx.Response.Header.Peek("Access-Control-Allow-Origin")); got != "http://localhost:3000" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q", path, got)
		}
	}
}
