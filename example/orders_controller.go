package main

import (
	"time"

	"github.com/axkit/errors"
	"github.com/axkit/vatel"
	"github.com/axkit/vatel/auth"
	"github.com/valyala/fasthttp"
)

// Order is the domain model returned by order endpoints.
type Order struct {
	ID         int       `json:"id"`
	CustomerID int       `json:"customerId"`
	Product    string    `json:"product"`
	Quantity   int       `json:"quantity"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"createdAt"`
}

var errOrderNotFound = errors.Template("order not found").StatusCode(404)

// in-memory store for example purposes — replace with a real repository.
var orderStore = []Order{
	{ID: 1, CustomerID: 100, Product: "Widget A", Quantity: 5, Status: "pending", CreatedAt: time.Now()},
	{ID: 2, CustomerID: 101, Product: "Widget B", Quantity: 3, Status: "shipped", CreatedAt: time.Now()},
}

// ── List Orders ───────────────────────────────────────────────────────────────
// GET /orders?status=pending&page=1&limit=20

type ListOrdersController struct {
	vatel.QueryParams[struct {
		Status string `param:"status"`
		Page   int    `param:"page"`
		Limit  int    `param:"limit"`
	}]
	vatel.Output[[]Order]
}

func (c *ListOrdersController) Handle(_ vatel.Context) error {
	result := make([]Order, 0, len(orderStore))
	for _, o := range orderStore {
		if c.Query.Status == "" || o.Status == c.Query.Status {
			result = append(result, o)
		}
	}
	c.Out = result
	return nil
}

// ── Get Order ─────────────────────────────────────────────────────────────────
// GET /orders/{id}

type GetOrderController struct {
	vatel.PathParams[struct {
		ID int `param:"id"`
	}]
	vatel.Output[Order]
}

func (c *GetOrderController) Handle(_ vatel.Context) error {
	for _, o := range orderStore {
		if o.ID == c.Params.ID {
			c.Out = o
			return nil
		}
	}
	return errOrderNotFound.New().Set("id", c.Params.ID)
}

// ── Create Order ──────────────────────────────────────────────────────────────
// POST /orders

type CreateOrderController struct {
	vatel.Body[struct {
		CustomerID int    `json:"customerId"`
		Product    string `json:"product"`
		Quantity   int    `json:"quantity"`
	}]
	vatel.Output[Order]
}

func (c *CreateOrderController) Handle(vatel.Context) error {
	o := Order{
		ID:         len(orderStore) + 1,
		CustomerID: c.In.CustomerID,
		Product:    c.In.Product,
		Quantity:   c.In.Quantity,
		Status:     "pending",
		CreatedAt:  time.Now(),
	}
	orderStore = append(orderStore, o)
	c.Out = o
	return nil
}

// ── Update Order ──────────────────────────────────────────────────────────────
// PUT /orders/{id}

type UpdateOrderController struct {
	vatel.PathParams[struct {
		ID int `param:"id"`
	}]
	vatel.Body[struct {
		Product  string `json:"product"`
		Quantity int    `json:"quantity"`
		Status   string `json:"status"`
	}]
	vatel.Output[Order]
}

func (c *UpdateOrderController) Handle(_ vatel.Context) error {
	for i := range orderStore {
		if orderStore[i].ID == c.Params.ID {
			orderStore[i].Product = c.In.Product
			orderStore[i].Quantity = c.In.Quantity
			orderStore[i].Status = c.In.Status
			c.Out = orderStore[i]
			return nil
		}
	}
	return errOrderNotFound.New().Set("id", c.Params.ID)
}

// ── Delete Order ──────────────────────────────────────────────────────────────
// DELETE /orders/{id}   — no response body, returns 204

type DeleteOrderController struct {
	vatel.PathParams[struct {
		ID int `param:"id"`
	}]
}

func (c *DeleteOrderController) Handle(_ vatel.Context) error {
	for i, o := range orderStore {
		if o.ID == c.Params.ID {
			orderStore = append(orderStore[:i], orderStore[i+1:]...)
			return nil
		}
	}
	return errOrderNotFound.New().Set("id", c.Params.ID)
}

// ── Endpoint Registration ─────────────────────────────────────────────────────

func OrderEndpoints(cfg *auth.Config) vatel.Endpoints {
	read := cfg.RequirePerms(map[string]uint{"orders:read": Permissions["orders:read"]})
	write := cfg.RequirePerms(map[string]uint{"orders:write": Permissions["orders:write"]})

	return vatel.Endpoints{
		vatel.GET("/orders", func() vatel.Handler { return &ListOrdersController{} }, read),
		vatel.GET("/orders/{id}", func() vatel.Handler { return &GetOrderController{} }, read),
		vatel.POST("/orders", func() vatel.Handler { return &CreateOrderController{} }, write,
			vatel.WithSuccessStatusCode(fasthttp.StatusCreated)),
		vatel.PUT("/orders/{id}", func() vatel.Handler { return &UpdateOrderController{} }, write),
		vatel.DELETE("/orders/{id}", func() vatel.Handler { return &DeleteOrderController{} }, write,
			vatel.WithSuccessStatusCode(fasthttp.StatusNoContent)),
	}
}
