package vatel

import (
	"bufio"
	"context"
	stderrors "errors"
	"net"
	"testing"
	"time"

	"github.com/fasthttp/router"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

// waitServing waits until v has a server started by serve.
func waitServing(t *testing.T, v *Vatel) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v.mu.Lock()
		started := v.srv != nil
		v.mu.Unlock()
		if started {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("server did not start")
}

// startServer builds eps and serves them on an in-memory listener. It returns
// the listener and the channel of the serve result.
func startServer(t *testing.T, v *Vatel, eps Endpoints) (*fasthttputil.InmemoryListener, <-chan error) {
	t.Helper()
	v.Add(eps)
	mux := router.New()
	if err := v.BuildHandlers(mux, discardLogger()); err != nil {
		t.Fatalf("BuildHandlers: %v", err)
	}
	ln := fasthttputil.NewInmemoryListener()
	served := make(chan error, 1)
	go func() { served <- v.serve(ln, mux) }()
	waitServing(t, v)
	return ln, served
}

// roundTrip writes a keep-alive GET request to conn and reads the response.
func roundTrip(t *testing.T, conn net.Conn, br *bufio.Reader, path string) *fasthttp.Response {
	t.Helper()
	if _, err := conn.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: test\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	var resp fasthttp.Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("read response: %v", err)
	}
	return &resp
}

func TestShutdown_IdleKeepAliveConnection(t *testing.T) {
	v := NewVatel()
	var reqCtx context.Context
	ln, served := startServer(t, v, Endpoints{GET("/ok", Func(func(ctx Context) error {
		reqCtx = ctx.Context()
		return nil
	}))})

	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	if resp := roundTrip(t, conn, br, "/ok"); resp.StatusCode() != 200 || resp.ConnectionClose() {
		t.Fatalf("status = %d, close = %v; want a 200 keep-alive response", resp.StatusCode(), resp.ConnectionClose())
	}

	// The connection stays open and idle: Shutdown must not wait for it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := v.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Shutdown took %v with an idle keep-alive connection", d)
	}
	if err := <-served; err != nil {
		t.Errorf("serve = %v, want nil", err)
	}
	if reqCtx.Err() == nil {
		t.Error("base context must be cancelled after Shutdown")
	}
}

func TestShutdown_InFlightRequestFinishes(t *testing.T) {
	v := NewVatel()
	started := make(chan struct{})
	release := make(chan struct{})
	var ctxErr error
	ln, served := startServer(t, v, Endpoints{GET("/slow", Func(func(ctx Context) error {
		close(started)
		<-release
		ctxErr = ctx.Context().Err()
		return nil
	}))})

	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	respc := make(chan *fasthttp.Response, 1)
	go func() {
		if _, err := conn.Write([]byte("GET /slow HTTP/1.1\r\nHost: test\r\n\r\n")); err != nil {
			respc <- nil
			return
		}
		var resp fasthttp.Response
		if err := resp.Read(br); err != nil {
			respc <- nil
			return
		}
		respc <- &resp
	}()
	<-started

	shutdown := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdown <- v.Shutdown(ctx)
	}()

	select {
	case err := <-shutdown:
		t.Fatalf("Shutdown returned %v while a request is in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	if err := <-shutdown; err != nil {
		t.Errorf("Shutdown = %v, want nil", err)
	}
	resp := <-respc
	if resp == nil || resp.StatusCode() != 200 {
		t.Fatalf("in-flight request: response %v, want 200", resp)
	}
	if !resp.ConnectionClose() {
		t.Error("a response during shutdown must carry Connection: close")
	}
	if ctxErr != nil {
		t.Errorf("request context of an in-flight request cancelled before the deadline: %v", ctxErr)
	}
	if err := <-served; err != nil {
		t.Errorf("serve = %v, want nil", err)
	}
}

func TestShutdown_BeforeListenAndServe(t *testing.T) {
	v := NewVatel()
	if err := v.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	errc := make(chan error, 1)
	go func() { errc <- v.ListenAndServe("127.0.0.1:0", router.New(), discardLogger()) }()
	select {
	case err := <-errc:
		if !stderrors.Is(err, ErrServerClosed) {
			t.Errorf("ListenAndServe after Shutdown = %v, want ErrServerClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe after Shutdown started the server")
	}
}

// TestShutdown_RacesListenAndServe calls Shutdown while ListenAndServe starts:
// the server must stop in every ordering (run with -race).
func TestShutdown_RacesListenAndServe(t *testing.T) {
	for i := 0; i < 20; i++ {
		v := NewVatel()
		errc := make(chan error, 1)
		go func() { errc <- v.ListenAndServe("127.0.0.1:0", router.New(), discardLogger()) }()
		if i%2 == 1 {
			time.Sleep(time.Millisecond)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := v.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown = %v", err)
		}
		cancel()
		select {
		case err := <-errc:
			if err != nil && !stderrors.Is(err, ErrServerClosed) {
				t.Errorf("ListenAndServe = %v, want nil or ErrServerClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("server kept running after Shutdown")
		}
	}
}

func TestServerConfig(t *testing.T) {
	v := NewVatel(WithServerConfig(func(s *fasthttp.Server) { s.Name = "vatel-test" }))
	ln, served := startServer(t, v, Endpoints{GET("/ok", Func(func(Context) error { return nil }))})

	v.mu.Lock()
	idle, closeOnShutdown := v.srv.IdleTimeout, v.srv.CloseOnShutdown
	v.mu.Unlock()
	if idle != DefaultIdleTimeout || !closeOnShutdown {
		t.Errorf("IdleTimeout = %v, CloseOnShutdown = %v; want %v, true", idle, closeOnShutdown, DefaultIdleTimeout)
	}

	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	resp := roundTrip(t, conn, bufio.NewReader(conn), "/ok")
	conn.Close()
	if got := string(resp.Header.Peek("Server")); got != "vatel-test" {
		t.Errorf("Server header = %q, want the configured name", got)
	}

	if err := v.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-served; err != nil {
		t.Errorf("serve = %v", err)
	}
}
