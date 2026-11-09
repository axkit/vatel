package vatel

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fasthttp/router"
	"github.com/valyala/fasthttp"
)

// patActorKey mirrors the typed context key pattern of an application
// (v0.14: ctx.RequestCtx().SetUserValue(key, v) + goctx.Value(key)).
type patActorKey struct{}

type txKey struct{}

// viaPersonalAccessToken is a business function that only sees a Go context.
func viaPersonalAccessToken(ctx context.Context) bool {
	id, ok := ctx.Value(patActorKey{}).(int)
	return ok && id != 0
}

// TestContext_Value_TypedKeyFromMiddleware: a value stored under a typed key
// by middleware (OnAuthorization, OnHandle) is returned by ctx.Context().Value
// in the handler, in a business function and through a derived context.
func TestContext_Value_TypedKeyFromMiddleware(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []EndpointOption
	}{
		{"no timeout", nil},
		{"timeout", []EndpointOption{WithTimeout(time.Hour)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := NewVatel()
			v.AddMiddleware(OnAuthorization, func(ctx Context, _ EndpointMeta) error {
				ctx.SetValue(patActorKey{}, 42)
				return nil
			})
			v.AddMiddleware(OnHandle, func(ctx Context, _ EndpointMeta) error {
				ctx.SetValue(txKey{}, "tx-1")
				return nil
			})

			var viaPAT, viaDerived bool
			var tx any
			var deadlineSet bool
			fctx := newGETCtx("/orgs")
			run(v, GET("/orgs", Func(func(ctx Context) error {
				goctx := ctx.Context()
				viaPAT = viaPersonalAccessToken(goctx)
				tx = goctx.Value(txKey{})
				derived, cancel := context.WithCancel(context.WithValue(goctx, "other", 1))
				defer cancel()
				viaDerived = viaPersonalAccessToken(derived)
				_, deadlineSet = goctx.Deadline()
				return nil
			}), tc.opts...), fctx)

			if got := responseStatus(fctx); got != 200 {
				t.Fatalf("want 200, got %d: %s", got, fctx.Response.Body())
			}
			if !viaPAT {
				t.Error("ctx.Context().Value(typed key) did not return the middleware value")
			}
			if !viaDerived {
				t.Error("a context derived from ctx.Context() did not see the middleware value")
			}
			if tx != "tx-1" {
				t.Errorf("OnHandle value = %v, want tx-1", tx)
			}
			if deadlineSet != (tc.opts != nil) {
				t.Errorf("Deadline set = %v, want %v", deadlineSet, tc.opts != nil)
			}
		})
	}
}

// TestContext_Value_StringKeysAndPathParams: values stored with Set, the
// request ID and router path params are returned by Context().Value, as by
// v0.14's RequestCtx.
func TestContext_Value_StringKeysAndPathParams(t *testing.T) {
	v := NewVatel(WithRequestID())
	v.AddMiddleware(OnAuthentication, func(ctx Context, _ EndpointMeta) error {
		ctx.Set("user", "alice")
		return nil
	})
	var user, id, reqID any
	v.Add(Endpoints{GET("/items/{id}", Func(func(ctx Context) error {
		user = ctx.Context().Value("user")
		id = ctx.Context().Value("id")
		reqID = ctx.Context().Value("reqId")
		return nil
	}))})
	mux := router.New()
	if err := v.BuildHandlers(mux, discardLogger()); err != nil {
		t.Fatalf("BuildHandlers: %v", err)
	}
	fctx := newGETCtx("/items/7")
	mux.Handler(fctx)

	if user != "alice" {
		t.Errorf("Value(user) = %v, want alice", user)
	}
	if id != "7" {
		t.Errorf("Value(id) = %v, want 7", id)
	}
	if reqID != fctx.ID() {
		t.Errorf("Value(reqId) = %v, want %d", reqID, fctx.ID())
	}
}

// TestContext_Value_Lazy: a context obtained before a value is stored sees it.
func TestContext_Value_Lazy(t *testing.T) {
	v := NewVatel()
	var early context.Context
	v.AddMiddleware(OnRequest, func(ctx Context, _ EndpointMeta) error {
		early = ctx.Context()
		return nil
	})
	v.AddMiddleware(OnAuthorization, func(ctx Context, _ EndpointMeta) error {
		ctx.SetValue(patActorKey{}, 7)
		return nil
	})
	var seen bool
	run(v, GET("/x", Func(func(ctx Context) error {
		ctx.Set("late", "v")
		seen = viaPersonalAccessToken(early) && early.Value("late") == "v"
		return nil
	})), newGETCtx("/x"))

	if !seen {
		t.Error("a context obtained before SetValue/Set did not see the values")
	}
}

// TestContext_Value_ParentFallback: keys without a request value are looked up
// in the parent context; a request value takes precedence.
func TestContext_Value_ParentFallback(t *testing.T) {
	parent := context.WithValue(context.WithValue(context.Background(), txKey{}, "parent-tx"), patActorKey{}, 1)
	c := newContext(&fasthttp.RequestCtx{}, parent)
	c.SetValue(patActorKey{}, 2)

	if got := c.Context().Value(txKey{}); got != "parent-tx" {
		t.Errorf("Value(txKey) = %v, want parent-tx", got)
	}
	if got := c.Context().Value(patActorKey{}); got != 2 {
		t.Errorf("Value(patActorKey) = %v, want the request value 2", got)
	}
	if got := c.Context().Value("missing"); got != nil {
		t.Errorf("Value(missing) = %v, want nil", got)
	}

	// NewContext (tests) supports SetValue too.
	ctx := NewContext(&fasthttp.RequestCtx{})
	ctx.SetValue(patActorKey{}, 3)
	if !viaPersonalAccessToken(ctx.Context()) {
		t.Error("NewContext: Context().Value did not return the SetValue value")
	}
}

// TestContext_Value_NotLeakedAfterRequest: fasthttp reuses RequestCtx, so a
// context kept after the request (e.g. by a goroutine) must not see the values
// of the next request on the same RequestCtx.
func TestContext_Value_NotLeakedAfterRequest(t *testing.T) {
	v := NewVatel()
	var kept context.Context
	fctx := newGETCtx("/x")
	run(v, GET("/x", Func(func(ctx Context) error {
		ctx.SetValue(patActorKey{}, 1)
		kept = ctx.Context()
		return nil
	})), fctx)

	if got := kept.Value(patActorKey{}); got != nil {
		t.Errorf("after the request Value = %v, want nil", got)
	}
	// The next request on the reused RequestCtx.
	fctx.Request.ResetUserValues()
	fctx.SetUserValue(patActorKey{}, 99)
	if got := kept.Value(patActorKey{}); got != nil {
		t.Errorf("Value of a finished request returned the next request's value %v", got)
	}
	if kept.Err() != nil {
		t.Errorf("Err() = %v, want nil (no timeout, no Shutdown)", kept.Err())
	}
}

// TestContext_Value_ConcurrentGoroutine: a goroutine started by the handler
// may read Context().Value while the request goroutine stores values and
// finishes (run with -race).
func TestContext_Value_ConcurrentGoroutine(t *testing.T) {
	v := NewVatel()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	fctx := newGETCtx("/x")
	run(v, GET("/x", Func(func(ctx Context) error {
		goctx := ctx.Context()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = goctx.Value(patActorKey{})
					_ = goctx.Value("k")
				}
			}
		}()
		for i := range 100 {
			ctx.SetValue(patActorKey{}, i)
			ctx.Set("k", i)
		}
		return nil
	})), fctx)
	// The server resets the user values after the handler returns.
	fctx.Request.ResetUserValues()
	close(stop)
	wg.Wait()
}

// TestContext_Value_TypedKeyNotLogged: values stored under non-string keys are
// not written to the request log; string keys are, as with Set.
func TestContext_Value_TypedKeyNotLogged(t *testing.T) {
	v := NewVatel()
	v.AddMiddleware(OnAuthorization, func(ctx Context, _ EndpointMeta) error {
		ctx.SetValue(txKey{}, "typed-secret")
		ctx.SetValue("visible", "string-key")
		return nil
	})
	_, _, raw := serveLogged(t, v, Endpoints{
		GET("/x", Func(func(Context) error { return nil }), WithLogOption(LogEnter|LogExit)),
	}, "GET", "/x")

	if strings.Contains(raw, "typed-secret") {
		t.Errorf("typed-key value written to the log: %s", raw)
	}
	if !strings.Contains(raw, "string-key") {
		t.Errorf("string-key value missing from the log: %s", raw)
	}
}
