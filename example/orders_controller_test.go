package main

import (
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/axkit/vatel"
	"github.com/axkit/vatel/auth"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

// useOrderStore replaces the order store of the handlers with one holding
// orders for the test.
func useOrderStore(t *testing.T, orders ...Order) *OrderStore {
	t.Helper()
	prev := orderStore
	orderStore = NewOrderStore(orders...)
	t.Cleanup(func() { orderStore = prev })
	return orderStore
}

// ordersAPI serves the order endpoints with the example auth; cfg may switch
// the checks off (Disabled).
type ordersAPI struct {
	t   *testing.T
	mux *router.Router
	cfg *auth.Config
}

func newOrdersAPI(t *testing.T) *ordersAPI {
	t.Helper()
	cfg := &auth.Config{Decoder: &JWTDecoder{Secret: testSecret}, Authorizer: &BitsetAuthorizer{}}
	v := vatel.NewVatel()
	v.AddMiddleware(vatel.OnAuthentication, cfg.Middleware())
	v.Add(OrderEndpoints(cfg))
	mux := router.New()
	l := zerolog.Nop()
	if err := v.BuildHandlers(mux, &l); err != nil {
		t.Fatal(err)
	}
	return &ordersAPI{t: t, mux: mux, cfg: cfg}
}

// token returns a token of user 100 in the organization org with every
// permission.
func (a *ordersAPI) token(org int) string {
	return mustSign(a.t, testSecret, Claims{UserID: 100, OrgID: org, PermBytes: AllPerms(),
		Exp: time.Now().Add(time.Hour).Unix()})
}

// do sends a request with the token (none if empty) and returns the status
// and the body of the response.
func (a *ordersAPI) do(method, path, token, body string) (int, []byte) {
	var fctx fasthttp.RequestCtx
	fctx.Request.Header.SetMethod(method)
	fctx.Request.SetRequestURI(path)
	if token != "" {
		fctx.Request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		fctx.Request.Header.SetContentType("application/json")
		fctx.Request.SetBodyString(body)
	}
	a.mux.Handler(&fctx)
	return fctx.Response.StatusCode(), append([]byte(nil), fctx.Response.Body()...)
}

// The organization comes only from the token: an organization lists, reads,
// changes and deletes only its own orders (another's is not found, 404, and
// is not changed), and creates orders in its own organization whatever the
// body says.
func TestOrders_OrganizationIsolation(t *testing.T) {
	s := useOrderStore(t,
		Order{ID: 1, OrgID: 1, Product: "A", Status: "pending"},
		Order{ID: 2, OrgID: 2, Product: "B", Status: "pending"},
		Order{ID: 3, OrgID: 1, Product: "C", Status: "shipped"},
	)
	a := newOrdersAPI(t)
	org1, org2 := a.token(1), a.token(2)

	st, body := a.do(fasthttp.MethodGet, "/orders", org1, "")
	var list []Order
	if err := json.Unmarshal(body, &list); st != fasthttp.StatusOK || err != nil {
		t.Fatalf("list: %d %s %v", st, body, err)
	}
	if len(list) != 2 || list[0].ID != 1 || list[1].ID != 3 {
		t.Errorf("list of organization 1 = %+v, want orders 1 and 3", list)
	}
	if st, body := a.do(fasthttp.MethodGet, "/orders?status=pending", org2, ""); st != fasthttp.StatusOK ||
		string(body) != `[{"id":2,"orgId":2,"customerId":0,"product":"B","quantity":0,"status":"pending","createdAt":"0001-01-01T00:00:00Z"}]` {
		t.Errorf("list of organization 2 = %d %s, want only order 2", st, body)
	}

	if st, _ := a.do(fasthttp.MethodGet, "/orders/1", org1, ""); st != fasthttp.StatusOK {
		t.Errorf("get own order: %d, want 200", st)
	}
	for _, r := range []struct{ method, body string }{
		{fasthttp.MethodGet, ""},
		{fasthttp.MethodPut, `{"product":"X","quantity":9,"status":"cancelled"}`},
		{fasthttp.MethodDelete, ""},
	} {
		if st, body := a.do(r.method, "/orders/2", org1, r.body); st != fasthttp.StatusNotFound {
			t.Errorf("%s order of another organization: %d %s, want 404", r.method, st, body)
		}
	}
	if o, ok := s.Get(2, 2); !ok || o.Product != "B" || o.Status != "pending" {
		t.Errorf("order of organization 2 after requests of organization 1 = %+v, %v; want unchanged", o, ok)
	}

	st, body = a.do(fasthttp.MethodPost, "/orders", org2, `{"product":"D","quantity":1,"orgId":1}`)
	var created Order
	if err := json.Unmarshal(body, &created); st != fasthttp.StatusCreated || err != nil {
		t.Fatalf("create: %d %s %v", st, body, err)
	}
	if created.OrgID != 2 {
		t.Errorf("created order organization = %d, want 2 (from the token)", created.OrgID)
	}
	path := "/orders/" + strconv.Itoa(created.ID)
	if st, _ := a.do(fasthttp.MethodGet, path, org1, ""); st != fasthttp.StatusNotFound {
		t.Errorf("get the new order with organization 1: %d, want 404", st)
	}
	if st, _ := a.do(fasthttp.MethodPut, path, org2, `{"product":"D","quantity":2,"status":"shipped"}`); st != fasthttp.StatusOK {
		t.Errorf("update own order: %d, want 200", st)
	}
	if st, _ := a.do(fasthttp.MethodDelete, path, org2, ""); st != fasthttp.StatusNoContent {
		t.Errorf("delete own order: %d, want 204", st)
	}
	if _, ok := s.Get(2, created.ID); ok {
		t.Error("own order not deleted")
	}
}

// Without a token payload (auth switched off) the handlers reply 401 instead
// of panicking or serving another organization's orders.
func TestOrders_NoToken(t *testing.T) {
	useOrderStore(t, Order{ID: 1, OrgID: 1, Product: "A"})
	a := newOrdersAPI(t)
	a.cfg.Disabled = true
	for _, r := range []struct{ method, path, body string }{
		{fasthttp.MethodGet, "/orders", ""},
		{fasthttp.MethodGet, "/orders/1", ""},
		{fasthttp.MethodPost, "/orders", `{"product":"D"}`},
		{fasthttp.MethodPut, "/orders/1", `{"product":"X"}`},
		{fasthttp.MethodDelete, "/orders/1", ""},
	} {
		if st, body := a.do(r.method, r.path, "", r.body); st != fasthttp.StatusUnauthorized {
			t.Errorf("%s %s without a token: %d %s, want 401", r.method, r.path, st, body)
		}
	}
	if o, ok := orderStore.Get(1, 1); !ok || o.Product != "A" {
		t.Errorf("order after requests without a token = %+v, %v; want unchanged", o, ok)
	}
}

// Concurrent handlers must not race on the store or hand out duplicate IDs
// (run with -race).
func TestOrders_Concurrent(t *testing.T) {
	useOrderStore(t, Order{ID: 1, OrgID: 1, Product: "A", Status: "pending"})
	a := newOrdersAPI(t)
	tok := a.token(1)
	const n = 50
	ids := make(chan int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(4)
		go func() {
			defer wg.Done()
			st, body := a.do(fasthttp.MethodPost, "/orders", tok, `{"product":"p"}`)
			var o Order
			if err := json.Unmarshal(body, &o); st != fasthttp.StatusCreated || err != nil {
				t.Errorf("create: %d %s %v", st, body, err)
			}
			ids <- o.ID
		}()
		go func() {
			defer wg.Done()
			a.do(fasthttp.MethodGet, "/orders", tok, "")
		}()
		go func() {
			defer wg.Done()
			a.do(fasthttp.MethodPut, "/orders/1", tok, `{"status":"shipped"}`)
		}()
		go func() {
			defer wg.Done()
			a.do(fasthttp.MethodDelete, "/orders/"+strconv.Itoa(1000+i), tok, "") // unknown: 404
		}()
	}
	wg.Wait()
	close(ids)

	seen := map[int]bool{}
	for id := range ids {
		if seen[id] {
			t.Errorf("duplicate order ID %d", id)
		}
		seen[id] = true
	}
}

// An ID freed by a delete is not reused.
func TestOrderStore_IDNotReusedAfterDelete(t *testing.T) {
	s := NewOrderStore()
	o1 := s.Create(Order{OrgID: 1})
	if !s.Delete(1, o1.ID) {
		t.Fatal("delete of the created order = false")
	}
	if o2 := s.Create(Order{OrgID: 1}); o2.ID == o1.ID {
		t.Errorf("ID %d reused after delete", o2.ID)
	}
}
