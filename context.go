package vatel

import (
	"context"
	"io"
	"mime/multipart"
	"sync"
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
	// SetBodyStream sets the response body to r, read only while the response
	// is sent, so a large file is never held in memory. size is the body
	// length (Content-Length) or -1 if unknown (chunked encoding). r is closed
	// after the response is sent if it is an io.Closer. An error returned by
	// the handler, a panic or a non-nil Resulter result replaces the stream
	// (and closes it).
	SetBodyStream(r io.Reader, size int) Context
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

	// Per-request key-value store (for middleware ↔ handler communication).
	// Values stored with Set are written to the request log.
	Set(key string, val any) Context
	Get(key string) any
	// SetValue stores val under an arbitrary comparable key, e.g. an unexported
	// typed key of the application, so that Context().Value(key) returns it.
	// A string key is the same as Set; a value under a non-string key is not
	// returned by Get and is not written to the request log.
	SetValue(key, val any) Context
	VisitUserValues(func(key []byte, val any))

	// Context returns the request context. It is cancelled by Vatel.Shutdown
	// and, when the endpoint has a timeout (WithTimeout, WithDefaultTimeout),
	// when the timeout expires or the request ends. fasthttp cannot detect a
	// client disconnect, so it is not cancelled when the client goes away.
	//
	// Its Value(key) returns the request values (Set, SetValue, path params,
	// "reqId"), looked up at call time, and then the values of the parent
	// context. After the request has ended it returns the parent values only.
	Context() context.Context
}

// VatelContext is the concrete Context implementation wrapping fasthttp.RequestCtx.
type VatelContext struct {
	fh         *fasthttp.RequestCtx
	tp         any
	verboseErr bool
	vctx       valueContext
	reported   bool // the request was reported to the MetricReporter

	// mu guards the user values of fh against Context().Value calls from other
	// goroutines, and ended, set when the request ends: fasthttp then resets
	// and reuses fh for another request.
	mu    sync.RWMutex
	ended bool
}

// valueContext is the Go context of a request: its parent's deadline and
// cancellation, and the request values before the parent's values.
type valueContext struct {
	context.Context
	c *VatelContext
}

func (v *valueContext) Value(key any) any {
	if val := v.c.userValue(key); val != nil {
		return val
	}
	return v.Context.Value(key)
}

func (v *valueContext) String() string { return "vatel.RequestContext" }

// NewContext creates a Context wrapping the given fasthttp request context.
// Its Context() is context.Background().
func NewContext(fctx *fasthttp.RequestCtx) Context {
	return newContext(fctx, context.Background())
}

// newContext creates a Context wrapping fctx with ctx as its Go context.
func newContext(fctx *fasthttp.RequestCtx, ctx context.Context) *VatelContext {
	c := &VatelContext{fh: fctx}
	c.vctx = valueContext{Context: ctx, c: c}
	return c
}

// userValue returns the request value under key, nil after the request ended.
func (c *VatelContext) userValue(key any) any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.ended {
		return nil
	}
	return c.fh.UserValue(key)
}

// end detaches Context().Value from the request values; called when the
// request ends, before fasthttp reuses the RequestCtx.
func (c *VatelContext) end() {
	c.mu.Lock()
	c.ended = true
	c.mu.Unlock()
}

func (c *VatelContext) BodyWriter() io.Writer {
	return c.fh.Response.BodyWriter()
}

func (c *VatelContext) SetBodyStream(r io.Reader, size int) Context {
	c.fh.Response.SetBodyStream(r, size)
	return c
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

func (c *VatelContext) Set(key string, val any) Context {
	return c.SetValue(key, val)
}

func (c *VatelContext) SetValue(key, val any) Context {
	c.mu.Lock()
	c.fh.SetUserValue(key, val)
	c.mu.Unlock()
	return c
}

func (c *VatelContext) Get(key string) any {
	return c.fh.UserValue(key)
}

func (c *VatelContext) VisitUserValues(f func(key []byte, val any)) {
	c.fh.VisitUserValues(f)
}

func (c *VatelContext) Context() context.Context {
	return &c.vctx
}
