package vatel

import "github.com/google/uuid"

type AccessTokenUpdateHandler struct {
	wv    *WebsocketVatel
	input struct {
		MessageID   uuid.UUID `json:"mid"`
		AccessToken string    `json:"at"`
	}
	output struct {
		Result    string    `json:"result"`
		MessageID uuid.UUID `json:"mid"`
	}
}

func (c *AccessTokenUpdateHandler) Input() interface{} {
	return &c.input
}

func (c *AccessTokenUpdateHandler) Result() interface{} {
	return &c.output
}

// Handle implements github.com/axkit/vatel Handler interface.
// The handler has no logic because if access token is
// invalid, middleware would not pass it to the handler.
func (c *AccessTokenUpdateHandler) Handle(ctx WebsocketContext) error {

	if err := c.wv.UpdateAccessToken(ctx.ID(), c.input.AccessToken); err != nil {
		return err
	}
	c.output.Result = "Ok"
	c.output.MessageID = c.input.MessageID
	return nil
}
