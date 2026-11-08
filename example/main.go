// Package main demonstrates a complete vatel API server setup.
// It shows JWT-based auth via vatel/auth, permission-gated endpoints,
// struct-based controllers with carrier embedding, CORS preflight and
// graceful shutdown.
//
// The listen address is taken from the ADDR environment variable
// (default ":8080").
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/axkit/vatel"
	"github.com/axkit/vatel/auth"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
)

func main() {
	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).
		With().Timestamp().Logger()

	authCfg := &auth.Config{
		Decoder:    &JWTDecoder{secret: os.Getenv("JWT_SECRET")},
		Authorizer: &BitsetAuthorizer{},
	}

	v := vatel.NewVatel(
		vatel.WithDefaultLogOption(vatel.LogFullOnExit),
		vatel.WithRequestID(),
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
	)

	mux := router.New()
	mux.GlobalOPTIONS = corsPreflight
	v.MustBuildHandlers(mux, &log)

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Info().Str("addr", addr).Msg("server started")
		if err := v.ListenAndServe(addr, mux, &log); err != nil {
			log.Fatal().Err(err).Msg("server stopped unexpectedly")
		}
	}()

	<-quit
	log.Info().Msg("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := v.Shutdown(ctx); err != nil {
		log.Error().Err(err).Msg("shutdown error")
	}
	log.Info().Msg("server stopped")
}
