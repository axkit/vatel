package vatel

import (
	"bufio"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
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

// testServer is a server started by startServer.
type testServer struct {
	dial   func() (net.Conn, error)
	served <-chan error // the serve result
}

// listeners are the listener kinds the shutdown tests run on: a real TCP
// listener (as used by ListenAndServe; fasthttp wraps its connections when
// MaxConnsPerIP is set) and an in-memory one.
var listeners = []string{"tcp", "inmemory"}

// startServer builds eps and serves them on a real TCP listener.
func startServer(t *testing.T, v *Vatel, eps Endpoints) testServer {
	t.Helper()
	return startServerOn(t, "tcp", v, eps)
}

// startServerOn builds eps and serves them on a listener of the given kind
// ("tcp" or "inmemory").
func startServerOn(t *testing.T, kind string, v *Vatel, eps Endpoints) testServer {
	t.Helper()
	v.Add(eps)
	mux := router.New()
	if err := v.BuildHandlers(mux, discardLogger()); err != nil {
		t.Fatalf("BuildHandlers: %v", err)
	}
	var ts testServer
	var ln net.Listener
	switch kind {
	case "tcp":
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr := l.Addr().String()
		ln, ts.dial = l, func() (net.Conn, error) { return net.Dial("tcp4", addr) }
	case "inmemory":
		l := fasthttputil.NewInmemoryListener()
		ln, ts.dial = l, l.Dial
	default:
		t.Fatalf("unknown listener kind %q", kind)
	}
	served := make(chan error, 1)
	go func() { served <- v.serve(ln, mux) }()
	waitServing(t, v)
	ts.served = served
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		v.Shutdown(ctx) //nolint:errcheck
	})
	return ts
}

// connect dials ts and sets a 10 s deadline on the connection.
func (ts testServer) connect(t *testing.T) net.Conn {
	t.Helper()
	conn, err := ts.dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	return conn
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

// waitOpenConns waits until the server started by serve has n open
// connections.
func waitOpenConns(t *testing.T, v *Vatel, n int32) {
	t.Helper()
	v.mu.Lock()
	srv := v.srv
	v.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for srv.GetOpenConnectionsCount() != n {
		if time.Now().After(deadline) {
			t.Fatalf("open connections = %d, want %d", srv.GetOpenConnectionsCount(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// get sends a GET request to path on conn; closeConn adds "Connection: close".
func get(t *testing.T, conn net.Conn, path string, closeConn bool) {
	t.Helper()
	req := "GET " + path + " HTTP/1.1\r\nHost: test\r\n"
	if closeConn {
		req += "Connection: close\r\n"
	}
	if _, err := conn.Write([]byte(req + "\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// readResponse reads a response from br.
func readResponse(t *testing.T, br *bufio.Reader) *fasthttp.Response {
	t.Helper()
	var resp fasthttp.Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("read response: %v", err)
	}
	return &resp
}

// assertClosed fails unless the server has closed the connection read by br.
func assertClosed(t *testing.T, br *bufio.Reader, what string) {
	t.Helper()
	if _, err := br.ReadByte(); err != io.EOF {
		t.Errorf("read from %s after Shutdown = %v, want io.EOF", what, err)
	}
}

// TestShutdown_IdleKeepAliveConnection: an idle keep-alive connection does
// not hold Shutdown up; it is closed instead of serving a later request with
// a cancelled context.
func TestShutdown_IdleKeepAliveConnection(t *testing.T) {
	for _, kind := range listeners {
		t.Run(kind, func(t *testing.T) {
			v := NewVatel()
			reqCtx := make(chan context.Context, 1)
			ts := startServerOn(t, kind, v, Endpoints{GET("/ok", Func(func(ctx Context) error {
				reqCtx <- ctx.Context()
				return nil
			}))})

			conn := ts.connect(t)
			br := bufio.NewReader(conn)
			if resp := roundTrip(t, conn, br, "/ok"); resp.StatusCode() != 200 || resp.ConnectionClose() {
				t.Fatalf("status = %d, close = %v; want a 200 keep-alive response", resp.StatusCode(), resp.ConnectionClose())
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			start := time.Now()
			if err := v.Shutdown(ctx); err != nil {
				t.Fatalf("Shutdown = %v, want nil", err)
			}
			if d := time.Since(start); d > time.Second {
				t.Errorf("Shutdown took %v with an idle keep-alive connection", d)
			}
			if err := <-ts.served; err != nil {
				t.Errorf("serve = %v, want nil", err)
			}
			if err := (<-reqCtx).Err(); err == nil {
				t.Error("base context must be cancelled after Shutdown")
			}
			assertClosed(t, br, "an idle connection")
		})
	}
}

// TestShutdown_InFlightRequestFinishes: Shutdown waits for a request being
// handled, which keeps a live context and gets "Connection: close".
func TestShutdown_InFlightRequestFinishes(t *testing.T) {
	for _, kind := range listeners {
		t.Run(kind, func(t *testing.T) {
			v := NewVatel()
			started := make(chan struct{})
			release := make(chan struct{})
			ctxErr := make(chan error, 1)
			ts := startServerOn(t, kind, v, Endpoints{GET("/slow", Func(func(ctx Context) error {
				close(started)
				<-release
				ctxErr <- ctx.Context().Err()
				return nil
			}))})

			conn := ts.connect(t)
			get(t, conn, "/slow", false)
			<-started

			shutdown := shutdownAsync(v)
			assertPending(t, shutdown, 100*time.Millisecond, "a request is in flight")
			close(release)

			resp := readResponse(t, bufio.NewReader(conn))
			if resp.StatusCode() != 200 || !resp.ConnectionClose() {
				t.Errorf("status = %d, close = %v; want 200 with Connection: close", resp.StatusCode(), resp.ConnectionClose())
			}
			if err := <-ctxErr; err != nil {
				t.Errorf("request context of an in-flight request cancelled before the deadline: %v", err)
			}
			if err := <-shutdown; err != nil {
				t.Errorf("Shutdown = %v, want nil", err)
			}
			if err := <-ts.served; err != nil {
				t.Errorf("serve = %v, want nil", err)
			}
		})
	}
}

// TestShutdown_Pipelining: two requests sent in one write; Shutdown starts
// while the second is handled. Both responses must arrive.
func TestShutdown_Pipelining(t *testing.T) {
	v := NewVatel()
	started := make(chan struct{})
	release := make(chan struct{})
	ctxErr := make(chan error, 1)
	ts := startServer(t, v, Endpoints{
		GET("/fast", Func(func(Context) error { return nil })),
		GET("/slow", Func(func(ctx Context) error {
			close(started)
			<-release
			ctxErr <- ctx.Context().Err()
			return nil
		})),
	})

	conn := ts.connect(t)
	if _, err := conn.Write([]byte("GET /fast HTTP/1.1\r\nHost: test\r\n\r\nGET /slow HTTP/1.1\r\nHost: test\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	<-started

	shutdown := shutdownAsync(v)
	assertPending(t, shutdown, 150*time.Millisecond, "a pipelined request is in flight")
	close(release)

	br := bufio.NewReader(conn)
	for i, path := range []string{"/fast", "/slow"} {
		resp := readResponse(t, br)
		if resp.StatusCode() != 200 {
			t.Errorf("%s: status = %d, want 200", path, resp.StatusCode())
		}
		if last := i == 1; last && !resp.ConnectionClose() {
			t.Errorf("%s: the last response must carry Connection: close", path)
		}
	}
	if err := <-ctxErr; err != nil {
		t.Errorf("request context of the pipelined request cancelled before the deadline: %v", err)
	}
	if err := <-shutdown; err != nil {
		t.Errorf("Shutdown = %v, want nil", err)
	}
	if err := <-ts.served; err != nil {
		t.Errorf("serve = %v, want nil", err)
	}
}

// TestShutdown_MaxConnsPerIP: with MaxConnsPerIP set (fasthttp then wraps
// every TCP connection), closed connections are released and Shutdown
// returns nil promptly when nothing is in flight.
func TestShutdown_MaxConnsPerIP(t *testing.T) {
	const limit = 5
	v := NewVatel(WithServerConfig(func(s *fasthttp.Server) { s.MaxConnsPerIP = limit }))
	ts := startServer(t, v, Endpoints{GET("/ok", Func(func(Context) error { return nil }))})

	// More connections in turn than the limit: each closed one must free
	// its slot, otherwise the later ones are rejected.
	for i := 0; i < 4*limit; i++ {
		conn := ts.connect(t)
		get(t, conn, "/ok", true)
		raw, _ := io.ReadAll(conn)
		conn.Close()
		if !strings.HasPrefix(string(raw), "HTTP/1.1 200") {
			t.Fatalf("connection %d: response %q, want 200", i, raw)
		}
	}
	waitOpenConns(t, v, 0)

	idle := ts.connect(t)
	br := bufio.NewReader(idle)
	roundTrip(t, idle, br, "/ok")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := v.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Shutdown took %v with nothing in flight", d)
	}
	if err := <-ts.served; err != nil {
		t.Errorf("serve = %v, want nil", err)
	}
	assertClosed(t, br, "an idle connection")
}

// TestShutdown_AfterDeadline: a request still running when the Shutdown
// deadline and the grace period have passed is not interrupted; its
// response then carries "Connection: close" and the connection is closed.
func TestShutdown_AfterDeadline(t *testing.T) {
	v := NewVatel()
	started := make(chan struct{})
	release := make(chan struct{})
	ts := startServer(t, v, Endpoints{GET("/stuck", Func(func(Context) error {
		close(started)
		<-release
		return nil
	}))})

	conn := ts.connect(t)
	get(t, conn, "/stuck", false)
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := v.Shutdown(ctx); !stderrors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown = %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d < shutdownGrace || d > 100*time.Millisecond+shutdownGrace+time.Second {
		t.Errorf("Shutdown took %v, want about the deadline plus %v", d, shutdownGrace)
	}
	close(release)

	br := bufio.NewReader(conn)
	resp := readResponse(t, br)
	if resp.StatusCode() != 200 || !resp.ConnectionClose() {
		t.Errorf("status = %d, close = %v; want 200 with Connection: close", resp.StatusCode(), resp.ConnectionClose())
	}
	assertClosed(t, br, "the connection of a request finished after Shutdown")
}

// TestShutdown_ConcurrentCalls: each Shutdown call honours its own context.
func TestShutdown_ConcurrentCalls(t *testing.T) {
	v := NewVatel()
	started := make(chan struct{})
	release := make(chan struct{})
	ts := startServer(t, v, Endpoints{GET("/stuck", Func(func(Context) error {
		close(started)
		<-release
		return nil
	}))})
	conn := ts.connect(t)
	get(t, conn, "/stuck", false)
	<-started

	long := shutdownAsync(v)
	assertPending(t, long, 50*time.Millisecond, "a request is in flight")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := v.Shutdown(ctx); !stderrors.Is(err, context.DeadlineExceeded) {
		t.Errorf("second Shutdown = %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond+shutdownGrace+time.Second {
		t.Errorf("second Shutdown took %v, want about its deadline plus %v", d, shutdownGrace)
	}
	close(release)
	if err := <-long; err != nil {
		t.Errorf("first Shutdown = %v, want nil", err)
	}
	if err := v.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown after the server stopped = %v, want nil", err)
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
	ts := startServer(t, v, Endpoints{GET("/ok", Func(func(Context) error { return nil }))})

	v.mu.Lock()
	idle, closeOnShutdown := v.srv.IdleTimeout, v.srv.CloseOnShutdown
	v.mu.Unlock()
	if idle != DefaultIdleTimeout || !closeOnShutdown {
		t.Errorf("IdleTimeout = %v, CloseOnShutdown = %v; want %v, true", idle, closeOnShutdown, DefaultIdleTimeout)
	}

	conn := ts.connect(t)
	resp := roundTrip(t, conn, bufio.NewReader(conn), "/ok")
	conn.Close()
	if got := string(resp.Header.Peek("Server")); got != "vatel-test" {
		t.Errorf("Server header = %q, want the configured name", got)
	}

	if err := v.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-ts.served; err != nil {
		t.Errorf("serve = %v", err)
	}
}

// shutdownAsync runs Shutdown with a 5 s deadline in a goroutine.
func shutdownAsync(v *Vatel) <-chan error {
	errc := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		errc <- v.Shutdown(ctx)
	}()
	return errc
}

// assertPending fails if Shutdown returns within d.
func assertPending(t *testing.T, shutdown <-chan error, d time.Duration, what string) {
	t.Helper()
	select {
	case err := <-shutdown:
		t.Fatalf("Shutdown returned %v while %s", err, what)
	case <-time.After(d):
	}
}

// TestShutdown_RequestBeingReceived: a request whose headers or body are
// still being received when Shutdown starts is served with a live context,
// and Shutdown waits for it.
func TestShutdown_RequestBeingReceived(t *testing.T) {
	body := `{"name":"bbbbbbbbbbbbbbbbbbbb"}`
	req := fmt.Sprintf("POST /x HTTP/1.1\r\nHost: test\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	cases := map[string]int{
		"headers": strings.Index(req, "Content-Type"),
		"body":    strings.Index(req, "\r\n\r\n") + 4 + 5,
	}
	for name, split := range cases {
		t.Run(name, func(t *testing.T) {
			v := NewVatel()
			handled := make(chan error, 1)
			ts := startServer(t, v, Endpoints{POST("/x", Func(func(ctx Context) error {
				handled <- ctx.Context().Err()
				return nil
			}))})

			conn := ts.connect(t)
			if _, err := conn.Write([]byte(req[:split])); err != nil {
				t.Fatalf("write: %v", err)
			}
			time.Sleep(50 * time.Millisecond) // let the server read the first part

			shutdown := shutdownAsync(v)
			assertPending(t, shutdown, 150*time.Millisecond, "a request is being received")

			if _, err := conn.Write([]byte(req[split:])); err != nil {
				t.Fatalf("write: %v", err)
			}
			resp := readResponse(t, bufio.NewReader(conn))
			if resp.StatusCode() != 200 || !resp.ConnectionClose() {
				t.Errorf("status = %d, close = %v; want 200 with Connection: close", resp.StatusCode(), resp.ConnectionClose())
			}
			if ctxErr := <-handled; ctxErr != nil {
				t.Errorf("request context cancelled before the Shutdown deadline: %v", ctxErr)
			}
			if err := <-shutdown; err != nil {
				t.Errorf("Shutdown = %v, want nil", err)
			}
			if err := <-ts.served; err != nil {
				t.Errorf("serve = %v, want nil", err)
			}
		})
	}
}

// TestShutdown_SilentNewConnection: a connection that sends nothing holds
// Shutdown up only for about 5 seconds (as in net/http), then it is closed.
func TestShutdown_SilentNewConnection(t *testing.T) {
	t.Parallel()
	v := NewVatel()
	ts := startServer(t, v, Endpoints{GET("/ok", Func(func(Context) error { return nil }))})
	conn := ts.connect(t)
	waitOpenConns(t, v, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
	defer cancel()
	start := time.Now()
	if err := v.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	if d := time.Since(start); d > 7*time.Second {
		t.Errorf("Shutdown took %v with a silent connection", d)
	}
	assertClosed(t, bufio.NewReader(conn), "a silent connection")
}

// TestShutdown_NewConnectionWithinGrace: a connection accepted before
// Shutdown that sends its request shortly after Shutdown started is served.
func TestShutdown_NewConnectionWithinGrace(t *testing.T) {
	v := NewVatel()
	ts := startServer(t, v, Endpoints{GET("/ok", Func(func(Context) error { return nil }))})
	conn := ts.connect(t)
	waitOpenConns(t, v, 1)

	shutdown := shutdownAsync(v)
	assertPending(t, shutdown, 100*time.Millisecond, "a new connection is within its grace period")

	resp := roundTrip(t, conn, bufio.NewReader(conn), "/ok")
	if resp.StatusCode() != 200 || !resp.ConnectionClose() {
		t.Errorf("status = %d, close = %v; want 200 with Connection: close", resp.StatusCode(), resp.ConnectionClose())
	}
	if err := <-shutdown; err != nil {
		t.Errorf("Shutdown = %v, want nil", err)
	}
	if err := <-ts.served; err != nil {
		t.Errorf("serve = %v, want nil", err)
	}
}

// TestServerConfig_ConnStateHook: a ConnState hook set by WithServerConfig
// is called.
func TestServerConfig_ConnStateHook(t *testing.T) {
	var mu sync.Mutex
	var states []fasthttp.ConnState
	v := NewVatel(WithServerConfig(func(s *fasthttp.Server) {
		s.ConnState = func(_ net.Conn, st fasthttp.ConnState) {
			mu.Lock()
			states = append(states, st)
			mu.Unlock()
		}
	}))
	ts := startServer(t, v, Endpoints{GET("/ok", Func(func(Context) error { return nil }))})
	conn := ts.connect(t)
	roundTrip(t, conn, bufio.NewReader(conn), "/ok")
	if err := v.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) == 0 || states[0] != fasthttp.StateNew {
		t.Errorf("user ConnState hook states = %v, want to start with StateNew", states)
	}
}

// A streamed body reaches the client whole, plain and gzipped (WithCompress),
// and the metric gets its size from Content-Length.
func TestServe_BodyStream(t *testing.T) {
	data := strings.Repeat("0123456789", 50_000)
	m := &syncMetrics{}
	v := NewVatel(WithMetricReporter(m))
	stream := func(ctx Context) error {
		ctx.SetContentType([]byte("application/octet-stream"))
		ctx.SetBodyStream(strings.NewReader(data), len(data))
		return nil
	}
	ts := startServer(t, v, Endpoints{
		GET("/file", Func(stream)),
		GET("/gz", Func(stream), WithCompress()),
	})
	conn := ts.connect(t)
	br := bufio.NewReader(conn)

	resp := roundTrip(t, conn, br, "/file")
	if resp.StatusCode() != 200 || string(resp.Body()) != data {
		t.Fatalf("/file: status %d, %d bytes, want 200 and %d bytes", resp.StatusCode(), len(resp.Body()), len(data))
	}

	if _, err := conn.Write([]byte("GET /gz HTTP/1.1\r\nHost: test\r\nAccept-Encoding: gzip\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	var gz fasthttp.Response
	if err := gz.Read(br); err != nil {
		t.Fatalf("read /gz: %v", err)
	}
	if ce := string(gz.Header.ContentEncoding()); ce != "gzip" {
		t.Fatalf("/gz Content-Encoding = %q, want gzip", ce)
	}
	body, err := gz.BodyGunzip()
	if err != nil || string(body) != data {
		t.Fatalf("/gz: gunzip err %v, %d bytes, want %d bytes", err, len(body), len(data))
	}

	for _, p := range []string{"/file", "/gz"} {
		if got := m.sizes(p); len(got) != 1 || got[0] != len(data) {
			t.Errorf("%s metric sizes = %v, want [%d]", p, got, len(data))
		}
	}
}
