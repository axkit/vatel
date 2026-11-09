package main

import (
	"slices"
	"sync"
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

// OrderStore is an in-memory order repository for example purposes; replace
// it with a real one. Handlers run concurrently, so every method takes the
// lock, and IDs come from a counter, never reused after a delete.
type OrderStore struct {
	mu     sync.Mutex
	orders []Order
	lastID int
}

// NewOrderStore returns a store holding orders; the next ID follows the
// largest of theirs.
func NewOrderStore(orders ...Order) *OrderStore {
	s := &OrderStore{orders: orders}
	for _, o := range orders {
		s.lastID = max(s.lastID, o.ID)
	}
	return s
}

// List returns a copy of the orders with the status, or all orders if status
// is empty.
func (s *OrderStore) List(status string) []Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Order, 0, len(s.orders))
	for _, o := range s.orders {
		if status == "" || o.Status == status {
			result = append(result, o)
		}
	}
	return result
}

// Get returns the order with id.
func (s *OrderStore) Get(id int) (Order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return Order{}, false
	}
	return s.orders[i], true
}

// Create assigns o a new ID, stores it and returns it.
func (s *OrderStore) Create(o Order) Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastID++
	o.ID = s.lastID
	s.orders = append(s.orders, o)
	return o
}

// Update calls f on the order with id and returns the updated order.
func (s *OrderStore) Update(id int, f func(*Order)) (Order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return Order{}, false
	}
	f(&s.orders[i])
	return s.orders[i], true
}

// Delete removes the order with id and reports whether it existed.
func (s *OrderStore) Delete(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return false
	}
	s.orders = slices.Delete(s.orders, i, i+1)
	return true
}

// index returns the position of the order with id, or -1. s.mu must be held.
func (s *OrderStore) index(id int) int {
	return slices.IndexFunc(s.orders, func(o Order) bool { return o.ID == id })
}

var orderStore = NewOrderStore(
	Order{ID: 1, CustomerID: 100, Product: "Widget A", Quantity: 5, Status: "pending", CreatedAt: time.Now()},
	Order{ID: 2, CustomerID: 101, Product: "Widget B", Quantity: 3, Status: "shipped", CreatedAt: time.Now()},
)

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
	c.Out = orderStore.List(c.Query.Status)
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
	o, ok := orderStore.Get(c.Params.ID)
	if !ok {
		return errOrderNotFound.New().Set("id", c.Params.ID)
	}
	c.Out = o
	return nil
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
	c.Out = orderStore.Create(Order{
		CustomerID: c.In.CustomerID,
		Product:    c.In.Product,
		Quantity:   c.In.Quantity,
		Status:     "pending",
		CreatedAt:  time.Now(),
	})
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
	o, ok := orderStore.Update(c.Params.ID, func(o *Order) {
		o.Product = c.In.Product
		o.Quantity = c.In.Quantity
		o.Status = c.In.Status
	})
	if !ok {
		return errOrderNotFound.New().Set("id", c.Params.ID)
	}
	c.Out = o
	return nil
}

// ── Delete Order ──────────────────────────────────────────────────────────────
// DELETE /orders/{id}   — no response body, returns 204

type DeleteOrderController struct {
	vatel.PathParams[struct {
		ID int `param:"id"`
	}]
}

func (c *DeleteOrderController) Handle(_ vatel.Context) error {
	if !orderStore.Delete(c.Params.ID) {
		return errOrderNotFound.New().Set("id", c.Params.ID)
	}
	return nil
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
