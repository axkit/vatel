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
	Set(key string, val any) WebsocketContext
	Get(key string) any
	VisitValues(func(key []byte, val any))
	Created() time.Time
	ID() uint64
}

// WsContext is the concrete WebsocketContext implementation.
type WsContext struct {
	kv      tinymap.TinyMap
	tp      any
	conn    *websocket.Conn
	w       io.Writer // the writer of conn, if set; see wsWriter
	created time.Time
}

// NewWsContext creates a WebsocketContext for the given connection.
func NewWsContext(c *websocket.Conn) WebsocketContext {
	return &WsContext{
		conn:    c,
		created: time.Now(),
	}
}

func (c *WsContext) SetTokenPayload(tp any) { c.tp = tp }
func (c *WsContext) TokenPayload() any      { return c.tp }
func (c *WsContext) ID() uint64             { return c.conn.ID() }
func (c *WsContext) Created() time.Time     { return c.created }

// BodyWriter returns the writer of messages to the client. Writes to it after
// the connection is closed fail instead of blocking.
func (c *WsContext) BodyWriter() io.Writer {
	if c.w != nil {
		return c.w
	}
	return c.conn
}

func (c *WsContext) Get(key string) any { return c.kv.Get(key) }

func (c *WsContext) Set(key string, val any) WebsocketContext {
	c.kv.Set(key, val)
	return c
}

func (c *WsContext) VisitValues(f func(key []byte, v any)) {
	c.kv.VisitValues(f)
}
