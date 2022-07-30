package vatel

import (
	"context"
	"io"
	"mime/multipart"

	"github.com/valyala/fasthttp"
)

type Context interface {
	BodyWriter() io.Writer
	SetContentType([]byte) Context
	SetStatusCode(code int) Context
	FormFile(key string) (*multipart.FileHeader, error)
	FormValue(key string) []byte
	SaveMultipartFile(fh *multipart.FileHeader, path string) error
	Header(name string) []byte
	TokenPayload() TokenPayloader
	SetTokenPayload(tp TokenPayloader)
	SetHeader(name, val []byte) Context
	RequestCtx() *fasthttp.RequestCtx
	Set(key string, val interface{}) Context
	Get(key string) interface{}
	VisitUserValues(func(key []byte, val interface{}))
}

type VatelContext struct {
	cancel context.CancelFunc
	fh     *fasthttp.RequestCtx
	kv     map[string]interface{}
	tp     TokenPayloader
}

func NewContext(ctx *fasthttp.RequestCtx) Context {
	c := VatelContext{
		fh: ctx,
	}
	return &c
}

func (ctx *VatelContext) SetTokenPayload(tp TokenPayloader) {
	ctx.tp = tp
}

func (ctx *VatelContext) TokenPayload() TokenPayloader {
	return ctx.tp
}

func (ctx *VatelContext) FormFile(key string) (*multipart.FileHeader, error) {
	return ctx.fh.FormFile(key)
}

func (ctx *VatelContext) FormValue(key string) []byte {
	return ctx.fh.FormValue(key)
}
func (ctx *VatelContext) Header(name string) []byte {
	return ctx.fh.Request.Header.Peek(name)
}

func (ctx *VatelContext) SaveMultipartFile(fh *multipart.FileHeader, path string) error {
	return fasthttp.SaveMultipartFile(fh, path)
}

func (ctx *VatelContext) SetContentType(contentType []byte) Context {
	ctx.fh.Response.Header.SetContentTypeBytes(contentType)
	return ctx
}

func (ctx *VatelContext) SetHeader(name, val []byte) Context {
	ctx.fh.Response.Header.SetBytesKV(name, val)
	return ctx
}

func (ctx *VatelContext) Log(key string, val interface{}) Context {
	if ctx.kv == nil {
		ctx.kv = make(map[string]interface{}, 1)
	}
	ctx.kv[key] = val
	return ctx
}

func (ctx *VatelContext) LogValues() map[string]interface{} {
	return ctx.kv
}

//
func (ctx *VatelContext) BodyWriter() io.Writer {
	return ctx.fh.Response.BodyWriter()
}

// SetStatusCode sets HTTP status code.
func (ctx *VatelContext) SetStatusCode(code int) Context {
	ctx.fh.SetStatusCode(code)
	return ctx
}

// RequestCtx returns fasthttp's context.
func (ctx *VatelContext) RequestCtx() *fasthttp.RequestCtx {
	return ctx.fh
}

func (ctx *VatelContext) Get(key string) interface{} {
	return ctx.fh.UserValue(key)
}

func (ctx *VatelContext) Set(key string, val interface{}) Context {
	ctx.fh.SetUserValue(key, val)
	return ctx
}

func (ctx *VatelContext) VisitUserValues(f func(key []byte, v interface{})) {
	ctx.fh.VisitUserValues(f)
}
