package vatel

import (
	"io"
	"time"

	"github.com/axkit/tinymap"
	"github.com/regorov/websocket"
)

// WebsocketContext is the per-message interface passed to WebSocket handlers
// and middleware.
type WebsocketContext interface {
	BodyWriter() io.Writer
	TokenPayload() any
	SetTokenPayload(tp any)
	Set(key string, val interface{}) WebsocketContext
	Get(key string) interface{}
	VisitValues(func(key []byte, val interface{}))
	Created() time.Time
	ID() uint64
}

// WsContext is the concrete WebsocketContext implementation.
type WsContext struct {
	kv      tinymap.TinyMap
	tp      any
	conn    *websocket.Conn
	created time.Time
}

// NewWsContext creates a WebsocketContext for the given connection.
func NewWsContext(c *websocket.Conn) WebsocketContext {
	return &WsContext{
		conn:    c,
		created: time.Now(),
	}
}

func (ctx *WsContext) SetTokenPayload(tp any) { ctx.tp = tp }
func (ctx *WsContext) TokenPayload() any      { return ctx.tp }
func (ctx *WsContext) ID() uint64             { return ctx.conn.ID() }
func (ctx *WsContext) Created() time.Time     { return ctx.created }

func (ctx *WsContext) BodyWriter() io.Writer { return ctx.conn }

func (ctx *WsContext) Get(key string) interface{} { return ctx.kv.Get(key) }

func (ctx *WsContext) Set(key string, val interface{}) WebsocketContext {
	ctx.kv.Set(key, val)
	return ctx
}

func (ctx *WsContext) VisitValues(f func(key []byte, v interface{})) {
	ctx.kv.VisitValues(f)
}
