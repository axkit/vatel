package main

import (
	"github.com/axkit/vatel"
	"github.com/valyala/fasthttp"
)

// CORS for browser clients.
//
// Preflight requests are answered by router.GlobalOPTIONS, which the router
// calls only for registered paths (an unknown path gets 404) and only when no
// explicit vatel.OPTIONS endpoint is registered for that path. The router has
// already set the Allow header with the path's methods. Do not put
// RequireAuth / RequirePerms on OPTIONS: browsers send preflight without
// the Authorization header.

const (
	corsAllowHeaders = "Authorization, Content-Type, X-Request-Id"
	corsMaxAge       = "600"
)

// corsPreflight answers CORS preflight requests for registered paths.
func corsPreflight(fctx *fasthttp.RequestCtx) {
	h := &fctx.Response.Header
	h.Set("Access-Control-Allow-Origin", corsOrigin(fctx.Request.Header.Peek("Origin")))
	h.Set("Access-Control-Allow-Methods", string(h.Peek("Allow")))
	h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
	h.Set("Access-Control-Max-Age", corsMaxAge)
	h.Add("Vary", "Origin")
	fctx.SetStatusCode(fasthttp.StatusNoContent)
}

// corsMiddleware adds the allow-origin header to actual (non-preflight)
// responses so browsers expose them to the calling page.
func corsMiddleware(ctx vatel.Context, _ vatel.EndpointMeta) error {
	ctx.SetHeader([]byte("Access-Control-Allow-Origin"), []byte(corsOrigin(ctx.Header("Origin"))))
	ctx.SetHeader([]byte("Vary"), []byte("Origin"))
	return nil
}

// corsOrigin returns the value for Access-Control-Allow-Origin. The example
// allows any origin; in production check origin against an allow list.
func corsOrigin(origin []byte) string {
	if len(origin) == 0 {
		return "*"
	}
	return string(origin)
}
