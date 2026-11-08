package vatel

import (
	"context"
	"io"
	"mime/multipart"
	"time"

	realip "github.com/axkit/fasthttp-realip"
	"github.com/valyala/fasthttp"
)

// Context is the per-request interface passed to every Handler, middleware,
// and response hook. It wraps the underlying HTTP request and response without
// exposing the fasthttp implementation.
type Context interface {
	// Response writing
	BodyWriter() io.Writer
	SetContentType([]byte) Context
	SetStatusCode(code int) Context
	SetHeader(name, val []byte) Context

	// Request reading
	Header(name string) []byte
	RemoteAddr() string
	RequestTime() time.Time
	RequestBody() []byte
	PathValue(name string) string
	QueryValue(name string) []byte

	// File handling
	FormFile(key string) (*multipart.FileHeader, error)
	FormValue(key string) []byte
	SaveMultipartFile(fh *multipart.FileHeader, path string) error

	// Auth — the payload set by auth middleware, nil if unauthenticated.
	// Handlers cast it to their concrete type: ctx.TokenPayload().(*MyClaims).
	TokenPayload() any
	SetTokenPayload(tp any)

	// VerboseError reports whether the auth middleware requested verbose error
	// responses for this request (e.g. for debug/admin tokens).
	VerboseError() bool
	SetVerboseError(bool)

	// Per-request key-value store (for middleware ↔ handler communication)
	Set(key string, val interface{}) Context
	Get(key string) interface{}
	VisitUserValues(func(key []byte, val interface{}))

	// Standard Go context for deadline/cancellation propagation.
	Context() context.Context
}

// VatelContext is the concrete Context implementation wrapping fasthttp.RequestCtx.
type VatelContext struct {
	fh         *fasthttp.RequestCtx
	tp         any
	verboseErr bool
	ctx        context.Context
}

// NewContext creates a Context wrapping the given fasthttp request context.
func NewContext(fctx *fasthttp.RequestCtx) Context {
	return &VatelContext{
		fh:  fctx,
		ctx: context.Background(),
	}
}

func (c *VatelContext) BodyWriter() io.Writer {
	return c.fh.Response.BodyWriter()
}

func (c *VatelContext) SetContentType(ct []byte) Context {
	c.fh.Response.Header.SetContentTypeBytes(ct)
	return c
}

func (c *VatelContext) SetStatusCode(code int) Context {
	c.fh.SetStatusCode(code)
	return c
}

func (c *VatelContext) SetHeader(name, val []byte) Context {
	c.fh.Response.Header.SetBytesKV(name, val)
	return c
}

func (c *VatelContext) Header(name string) []byte {
	return c.fh.Request.Header.Peek(name)
}

func (c *VatelContext) RemoteAddr() string {
	return realip.FromRequest(c.fh)
}

func (c *VatelContext) RequestTime() time.Time {
	return c.fh.Time()
}

func (c *VatelContext) RequestBody() []byte {
	return c.fh.Request.Body()
}

func (c *VatelContext) PathValue(name string) string {
	v := c.fh.UserValue(name)
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func (c *VatelContext) QueryValue(name string) []byte {
	return c.fh.QueryArgs().Peek(name)
}

func (c *VatelContext) FormFile(key string) (*multipart.FileHeader, error) {
	return c.fh.FormFile(key)
}

func (c *VatelContext) FormValue(key string) []byte {
	return c.fh.FormValue(key)
}

func (c *VatelContext) SaveMultipartFile(fh *multipart.FileHeader, path string) error {
	return fasthttp.SaveMultipartFile(fh, path)
}

func (c *VatelContext) TokenPayload() any      { return c.tp }
func (c *VatelContext) SetTokenPayload(tp any) { c.tp = tp }
func (c *VatelContext) VerboseError() bool     { return c.verboseErr }
func (c *VatelContext) SetVerboseError(v bool) { c.verboseErr = v }

func (c *VatelContext) Set(key string, val interface{}) Context {
	c.fh.SetUserValue(key, val)
	return c
}

func (c *VatelContext) Get(key string) interface{} {
	return c.fh.UserValue(key)
}

func (c *VatelContext) VisitUserValues(f func(key []byte, val interface{})) {
	c.fh.VisitUserValues(f)
}

func (c *VatelContext) Context() context.Context {
	return c.ctx
}
