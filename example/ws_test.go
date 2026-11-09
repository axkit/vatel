package main

import (
	stderrors "errors"
	"strings"
	"testing"
	"time"

	"github.com/axkit/errors"
	"github.com/axkit/vatel"
	"github.com/fasthttp/router"
	"github.com/regorov/websocket"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

func TestSameSecurityContext(t *testing.T) {
	cur := &Claims{UserID: 100, OrgID: 1}
	tests := []struct {
		name       string
		prev, next *Claims
		want       websocket.StatusCode // 0: accepted
	}{
		{"first token", nil, cur, 0},
		{"same context", cur, &Claims{UserID: 100, OrgID: 1}, 0},
		{"other organization", cur, &Claims{UserID: 100, OrgID: 2}, vatel.WsStatusContextChanged},
		{"other user", cur, &Claims{UserID: 101, OrgID: 1}, vatel.WsStatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var prev vatel.AuthPayload
			if tt.prev != nil {
				prev = tt.prev
			}
			err := sameSecurityContext(1, prev, tt.next)
			var wce *vatel.WsCloseError
			switch {
			case tt.want == 0 && err != nil:
				t.Errorf("err = %v, want nil", err)
			case tt.want != 0 && (!stderrors.As(err, &wce) || wce.Code != tt.want):
				t.Errorf("err = %v, want a WsCloseError %d", err, tt.want)
			}
		})
	}
}

func TestWsAuthorize(t *testing.T) {
	ctx := func(c *Claims) vatel.WebsocketContext {
		wctx := vatel.NewWsContext(nil)
		if c != nil {
			wctx.SetTokenPayload(c)
		}
		return wctx
	}
	read := []string{"orders:read"}
	tests := []struct {
		name  string
		ctx   vatel.WebsocketContext
		perms []string
		want  *errors.ErrorTemplate // nil: allowed
	}{
		{"no perms, no token", ctx(nil), nil, nil},
		{"perms, no token", ctx(nil), read, vatel.ErrUnauthorized},
		{"perms, token without them", ctx(&Claims{UserID: 1}), read, vatel.ErrForbidden},
		{"perms, token with them", ctx(&Claims{UserID: 1, PermBytes: AllPerms()}), read, nil},
		{"unknown perm", ctx(&Claims{UserID: 1, PermBytes: AllPerms()}), []string{"nope"}, vatel.ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := wsAuthorize(tt.ctx, "orders.watch", tt.perms)
			if (tt.want == nil && err != nil) || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// The organization of a watched order is checked against the token: an
// order of another organization is not found.
func TestWatchOrder_OrganizationFromToken(t *testing.T) {
	watch := func(c *Claims, id int) (Order, error) {
		ctx := vatel.NewWsContext(nil)
		if c != nil {
			ctx.SetTokenPayload(c)
		}
		h := &WatchOrderController{}
		h.In.ID = id
		err := h.Handle(ctx)
		return h.Out, err
	}
	if o, err := watch(&Claims{UserID: 100, OrgID: 1}, 1); err != nil || o.ID != 1 {
		t.Errorf("own order: %+v, %v", o, err)
	}
	if _, err := watch(&Claims{UserID: 100, OrgID: 1}, 2); !errors.Is(err, errOrderNotFound) {
		t.Errorf("order of another organization: err = %v, want not found", err)
	}
	if _, err := watch(nil, 1); !errors.Is(err, vatel.ErrUnauthorized) {
		t.Errorf("no token: err = %v, want ErrUnauthorized", err)
	}
}

func TestPrintDevToken_Org(t *testing.T) {
	if err := printDevToken(testSecret, []string{"x"}); err == nil {
		t.Error("printDevToken with a bad organization = nil, want an error")
	}
}

// End to end over the example wiring: a token, a watch of an order of the
// organization of the token, a refresh in the same context; then a token of
// another organization gets a 409 reply and the connection is closed with
// 4009.
func TestWebsocket_EndToEnd(t *testing.T) {
	wv := newWebsocketVatel(&JWTDecoder{Secret: testSecret})
	v := vatel.NewVatel(vatel.WithWebsocketSupport(wv))
	v.Add(WsEndpoints())
	mux := router.New()
	mux.GET("/ws", wv.Upgrade)
	l := zerolog.Nop()
	if err := v.BuildHandlers(mux, &l); err != nil {
		t.Fatal(err)
	}
	ln := fasthttputil.NewInmemoryListener()
	srv := &fasthttp.Server{Handler: mux.Handler}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })

	conn, err := ln.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	c, err := websocket.MakeClient(conn, "http://test/ws")
	if err != nil {
		t.Fatal(err)
	}
	fr := websocket.AcquireFrame()
	defer websocket.ReleaseFrame(fr)
	read := func() {
		t.Helper()
		fr.Reset()
		if _, err := c.ReadFrame(fr); err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	send := func(msg string) string {
		t.Helper()
		if _, err := c.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		read()
		if fr.IsClose() {
			t.Fatalf("closed with %d instead of a reply to %s", fr.Status(), msg)
		}
		return string(fr.Payload())
	}
	token := func(org int) string {
		tok := mustSign(t, testSecret, Claims{UserID: 100, OrgID: org, PermBytes: AllPerms(),
			Exp: time.Now().Add(time.Hour).Unix()})
		return `{"path":"updateAccessToken","data":{"mid":"00000000-0000-0000-0000-000000000001","at":"` + tok + `"}}`
	}

	if got := send(`{"path":"orders.watch","data":{"id":1}}`); !strings.Contains(got, `"statusCode":401`) {
		t.Errorf("watch without a token = %s, want 401", got)
	}
	if got := send(token(1)); !strings.Contains(got, `"result":"Ok"`) {
		t.Fatalf("token reply = %s", got)
	}
	if got := send(`{"path":"orders.watch","data":{"id":1}}`); !strings.Contains(got, `"orgId":1`) {
		t.Errorf("watch of an order of the organization = %s", got)
	}
	if got := send(`{"path":"orders.watch","data":{"id":2}}`); !strings.Contains(got, `"statusCode":404`) {
		t.Errorf("watch of an order of another organization = %s, want 404", got)
	}
	if got := send(token(1)); !strings.Contains(got, `"result":"Ok"`) {
		t.Errorf("refresh in the same context = %s", got)
	}
	if got := send(token(2)); !strings.Contains(got, `"code":"VTL-0009","statusCode":409`) {
		t.Errorf("token of another organization = %s, want 409 VTL-0009", got)
	}
	read()
	if !fr.IsClose() || fr.Status() != vatel.WsStatusContextChanged {
		t.Errorf("frame = close %v status %d, want close 4009", fr.IsClose(), fr.Status())
	}
}
