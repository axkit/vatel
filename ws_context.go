package vatel

import (
	"context"
	"io"
	"time"

	"github.com/axkit/tinymap"
	"github.com/dgrr/websocket"
)

type WebsocketContext interface {
	BodyWriter() io.Writer
	TokenPayload() TokenPayloader
	SetTokenPayload(tp TokenPayloader)
	Set(key string, val interface{}) WebsocketContext
	Get(key string) interface{}
	VisitValues(func(key []byte, val interface{}))
	Created() time.Time
}

type WsContext struct {
	cancel  context.CancelFunc
	kv      tinymap.TinyMap
	tp      TokenPayloader
	conn    *websocket.Conn
	created time.Time
}

func NewWsContext(c *websocket.Conn) WebsocketContext {
	return &WsContext{
		conn:    c,
		created: time.Now(),
	}
}

func (ctx *WsContext) SetTokenPayload(tp TokenPayloader) {
	ctx.tp = tp
}

func (ctx *WsContext) TokenPayload() TokenPayloader {
	return ctx.tp
}

// func (ctx *WsContext) Log(key string, val interface{}) *WsContext {
// 	if ctx.kv == nil {
// 		ctx.kv = make(map[string]interface{}, 1)
// 	}
// 	ctx.kv[key] = val
// 	return ctx
// }

// func (ctx *WsContext) LogValues() map[string]interface{} {
// 	return ctx.kv
// }
//

func (ctx *WsContext) BodyWriter() io.Writer {
	return ctx.conn
}

func (ctx *WsContext) Get(key string) interface{} {
	return ctx.kv.Get(key)
}

func (ctx *WsContext) Set(key string, val interface{}) WebsocketContext {
	ctx.kv.Set(key, val)
	return ctx
}

func (ctx *WsContext) VisitValues(f func(key []byte, v interface{})) {
	ctx.kv.VisitValues(f)
}

func (ctx *WsContext) Created() time.Time {
	return ctx.created
}
