package main

import (
	"time"

	"github.com/axkit/vatel"
)

// wsAuthTimeout is how long a WebSocket connection may stay without a token.
const wsAuthTimeout = 10 * time.Second

// newWebsocketVatel returns the WebSocket gateway of the example. A client
// sends its token in the built-in updateAccessToken message, within
// wsAuthTimeout after connecting, and again before it expires (Claims.Exp).
// A connection keeps one security context (user, organization): see
// sameSecurityContext.
func newWebsocketVatel(decoder vatel.TokenDecoder) *vatel.WebsocketVatel {
	wv := vatel.NewWebsocketVatel(
		vatel.WithTokenDecoder(decoder),
		vatel.WithWsAuthFunc(wsAuthorize),
		vatel.WithWsAuthTimeout(wsAuthTimeout),
	)
	wv.OnTokenUpdate(sameSecurityContext)
	return wv
}

// sameSecurityContext is the OnTokenUpdate policy: the first token of a
// connection sets its user and organization; a later token (a refresh) must
// have the same ones. A token of another organization closes the connection
// with 4009: the client opens a new connection with it and subscribes again.
// A token of another user closes it with 4003.
func sameSecurityContext(_ uint64, prev, next vatel.AuthPayload) error {
	if prev == nil {
		return nil
	}
	p, n := prev.(*Claims), next.(*Claims)
	switch {
	case p.UserID != n.UserID:
		return vatel.NewWsCloseError(vatel.WsStatusForbidden, "user changed", nil)
	case p.OrgID != n.OrgID:
		return vatel.NewWsCloseError(vatel.WsStatusContextChanged, "organization changed", nil)
	}
	return nil
}

// wsAuthorize checks the Perms of a WS endpoint against the token of the
// connection.
func wsAuthorize(ctx vatel.WebsocketContext, _ string, perms []string) error {
	if len(perms) == 0 {
		return nil
	}
	claims, _ := ctx.TokenPayload().(*Claims)
	if claims == nil {
		return vatel.ErrUnauthorized.New()
	}
	bits := make([]uint, 0, len(perms))
	for _, p := range perms {
		bit, ok := Permissions[p]
		if !ok {
			return vatel.ErrForbidden.New().Set("perm", p)
		}
		bits = append(bits, bit)
	}
	allowed, err := (&BitsetAuthorizer{}).IsAllowed(claims.Perms(), bits...)
	if err != nil {
		return err
	}
	if !allowed {
		return vatel.ErrForbidden.New()
	}
	return nil
}

// ── Watch Order ───────────────────────────────────────────────────────────────
// WS {"path": "orders.watch", "data": {"id": 1}}

// WatchOrderController subscribes the connection to an order (a demo: it
// replies with the order and sends no updates). The organization is taken
// from the token of the connection, never from the message: an order of
// another organization is reported as not found, so its existence is not
// disclosed.
type WatchOrderController struct {
	vatel.Body[struct {
		ID int `json:"id"`
	}]
	vatel.Output[Order]
}

func (c *WatchOrderController) Handle(ctx vatel.WebsocketContext) error {
	claims, _ := ctx.TokenPayload().(*Claims)
	if claims == nil {
		return vatel.ErrUnauthorized.New()
	}
	o, ok := orderStore.Get(claims.OrgID, c.In.ID)
	if !ok {
		return errOrderNotFound.New().Set("id", c.In.ID)
	}
	c.Out = o
	return nil
}

// WsEndpoints returns the WebSocket endpoints of the example.
func WsEndpoints() vatel.Endpoints {
	return vatel.Endpoints{
		vatel.WS("orders.watch", func() vatel.WebsocketHandler { return &WatchOrderController{} },
			vatel.Perms("orders:read")),
	}
}
