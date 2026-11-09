// Package main demonstrates a complete vatel API server setup.
// It shows JWT-based auth via vatel/auth, permission-gated endpoints,
// struct-based controllers with carrier embedding, a WebSocket endpoint with
// one security context (user, organization) per connection, CORS preflight
// and graceful shutdown.
//
// The listen address is taken from the ADDR environment variable
// (default ":8080"). Tokens are HS256 JWTs signed with the JWT_SECRET
// environment variable; without it a public development key is used.
//
// "go run ./example token [org]" prints a development token for user 100
// (alice) in organization org (default 1) with every permission, valid for
// 24 hours:
//
//	TOKEN=$(go run ./example token)
//	curl -H "Authorization: Bearer $TOKEN" localhost:8080/orders
//
// WebSocket clients connect to /ws, send the token within 10 seconds and
// then watch orders of their organization:
//
//	{"path": "updateAccessToken", "data": {"mid": "<uuid>", "at": "<token>"}}
//	{"path": "orders.watch", "data": {"id": 1}}
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/axkit/vatel"
	"github.com/axkit/vatel/auth"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
)

func main() {
	secret, devSecret := jwtSecret()
	if len(os.Args) > 1 && os.Args[1] == "token" {
		if err := printDevToken(secret, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).
		With().Timestamp().Logger()
	if devSecret {
		log.Warn().Msg(jwtSecretEnv + " is not set: tokens are signed with the public development key")
	}

	authCfg := &auth.Config{
		Decoder:    &JWTDecoder{Secret: secret},
		Authorizer: &BitsetAuthorizer{},
	}

	// WebSocket messages do not pass HTTP middleware: the token comes with
	// the updateAccessToken message and is checked by the WS gateway.
	wv := newWebsocketVatel(authCfg.Decoder)

	v := vatel.NewVatel(
		vatel.WithDefaultLogOption(vatel.LogFullOnExit),
		vatel.WithRequestID(),
		vatel.WithWebsocketSupport(wv),
	)

	// Decode the Authorization header and store the payload on every request.
	// Endpoints that require auth add RequireAuth / RequirePerms via authCfg.
	v.AddMiddleware(vatel.OnAuthentication, authCfg.Middleware())
	v.AddMiddleware(vatel.OnRequest, corsMiddleware)

	v.Add(
		HealthEndpoints(),
		OrderEndpoints(authCfg),
		UserEndpoints(authCfg),
		ImportEndpoints(authCfg),
		WsEndpoints(),
	)

	mux := router.New()
	mux.GlobalOPTIONS = corsPreflight
	mux.GET("/ws", wv.Upgrade)
	v.MustBuildHandlers(mux, &log)

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Info().Str("addr", addr).Msg("server started")
		// ListenAndServe returns nil after Shutdown, and ErrServerClosed
		// when the signal arrived before the server started.
		if err := v.ListenAndServe(addr, mux, &log); err != nil && !errors.Is(err, vatel.ErrServerClosed) {
			log.Fatal().Err(err).Msg("server stopped unexpectedly")
		}
	}()

	<-quit
	log.Info().Msg("shutting down...")

	// In-flight requests get up to 30 s to finish; then their contexts are
	// cancelled and Shutdown returns context.DeadlineExceeded.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := v.Shutdown(ctx); err != nil {
		log.Error().Err(err).Msg("shutdown error")
	}
	log.Info().Msg("server stopped")
}

// printDevToken prints a token for user 100 (alice) with every permission,
// valid for 24 hours, signed with secret, in the organization given by
// args[0] (default 1).
func printDevToken(secret []byte, args []string) error {
	org := 1
	if len(args) > 0 {
		var err error
		if org, err = strconv.Atoi(args[0]); err != nil {
			return fmt.Errorf("organization %q: %w", args[0], err)
		}
	}
	tok, err := SignToken(secret, Claims{
		UserID:    100,
		OrgID:     org,
		Login:     "alice",
		PermBytes: AllPerms(),
		Exp:       time.Now().Add(24 * time.Hour).Unix(),
	})
	if err != nil {
		return err
	}
	fmt.Println(tok)
	return nil
}
