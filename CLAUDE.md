# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build
go build -v ./...

# Run all tests; before committing -race, vet and gofmt must be clean
go test -race ./...
go vet ./...
gofmt -l .        # must print nothing

# Run a single test
go test -v -run TestName ./...

# Run tests in a specific package
go test -v ./jsonmask/...
go test -v ./auth/...

# Run the example server (port 8080 may be taken on the dev machine)
ADDR=:18080 go run ./example
# Mint a dev token for it (HS256, key from JWT_SECRET, public dev key if unset)
TOKEN=$(go run ./example token); curl -H "Authorization: Bearer $TOKEN" localhost:18080/orders

# Tidy dependencies
go mod tidy
```

## Architecture Overview

Vatel is a REST framework built on [fasthttp](https://github.com/valyala/fasthttp) and
fasthttp/router. Requires Go 1.26 (fasthttp v1.77.0 needs it), fasthttp v1.77.0+ and fasthttp/router
v1.5.4+ (`Shutdown` relies on fasthttp's `ShutdownWithContext`, see Core types), regorov/websocket v0.1.4+
(see WebSocket); CI reads the version
from `go.mod`. User docs: `README.md`; v0.14 → v1.25 changes: `MIGRATION.md`. Plans live in
`docs/plans/`, deferred work in `docs/backlog/`.

### Files

- `vatel.go` — `Vatel` (registry, `Option`s, `BuildHandlers`, `ListenAndServe`, `Shutdown`), DI interfaces.
- `options.go` — `EndpointOption`s and builders `GET/POST/PUT/PATCH/DELETE/HEAD/OPTIONS/WS`, `Endpoints`.
- `endpoint.go` — `Endpoint`, handler interfaces, request pipeline (`handler`, `buildHandler`), logging,
  error responses, `compile`, `LogOption` bits, sentinel errors (`VTL-0001`..`VTL-0008`).
- `decode.go` — path/query/body decoding with precomputed `fieldInfo`, `validate`, `Validator`.
- `carriers.go` — generic carriers `PathParams`, `QueryParams`, `Body`, `Output`, `OptionalOutput`, `Upload`, `Func`.
- `middleware.go` — `MiddlewarePos`, `EndpointMeta`.
- `context.go` — `Context` interface and `VatelContext` (wraps `fasthttp.RequestCtx`, not exposed).
- `ws_vatel.go`, `ws_context.go`, `ws_access_token_update.go` — WebSocket endpoints.
- `auth/` — JWT-style authentication/authorization middleware.
- `jsonmask/` — masking of JSON by `mask` struct tags.
- `vatel_c_toc.go` — `GET /` table of contents, registered automatically by `NewVatel`.
- `example/` — runnable app: auth (HS256 JWT verified with the standard library, `JWT_SECRET`), carriers,
  CORS, upload, graceful shutdown; its in-memory `OrderStore` is mutex-guarded (handlers run concurrently).

### Core types

**`Vatel`** — created by `NewVatel(opts...)`. `Add(Endpointer...)` collects endpoints;
`BuildHandlers(mux, logger)` compiles each endpoint and registers it with the router; router panics
(duplicate/conflicting route, own `GET /`) are returned as errors. It may be called once and copies the
middleware; after a failed build the `Vatel` is unusable ("previous build failed"), registration is not
transactional. `ListenAndServe` builds if needed and serves on a `fasthttp.Server` (`IdleTimeout` =
`DefaultIdleTimeout`, `CloseOnShutdown`, then `WithServerConfig`; vatel uses no `ConnState` hook, the
user's is left as is). `srv`/`ln`/`closed`/`stopped` are guarded by `v.mu`. `ln` is a `serverListener`
whose `Close` is idempotent.

`Shutdown(ctx)`: mark closed (a later `ListenAndServe` returns `ErrServerClosed`), close WS connections,
then `stopServer` once: close the listener (fasthttp's shutdown does nothing if `Serve` has not
registered it yet) and run `srv.ShutdownWithContext(context.Background())` in a goroutine that closes
`stopped`. fasthttp closes idle connections at once and new ones that sent nothing after 5 s, sets
`Connection: close` and waits until no connection is open; a connection is not idle from the first
byte of a request until its response is written (so requests still being received, pipelined requests
and `MaxConnsPerIP`-wrapped connections are covered). It is not given `ctx`: on expiry fasthttp resets its stop flag and forgets the
listeners, so leftover connections would keep serving and a second call would not wait. `Shutdown`
waits for `stopped` and the WS connections (`wsConns.drained`) or `ctx`: both done → cancel the base
context, return nil; `ctx` done → cancel the base context at once, wait up to `shutdownGrace` (1 s) for
`stopped`, return `ctx.Err()`. Without a server it cancels at once and waits for `drained` or `ctx`.
Concurrent calls share `stopped`, each with its own `ctx`.

**`Endpoint`** — embeds `EndpointMeta` (`Method`, `Path`, `Perms`, `LogOptions`) plus public config
(`Compress`, `ManualStatusCode`, `SuccessStatusCode`, `ResponseContentType`, `OptionalBody`, `Timeout`) and
a controller factory (`Controller func() Handler` or `WebsocketController`). Built with builders
(`vatel.GET(path, ctrl, opts...)`) and `EndpointOption`s (`Perms`, `WithLogOption`, `WithCompress`,
`WithSuccessStatusCode`, `WithManualStatusCode`, `WithContentType`, `WithOptionalBody`, `WithTimeout`).
`compile()` validates config (path begins with `/`, Paramer vs `{params}` in path, `OptionalBody` only on
POST/PUT/PATCH, known method) and precomputes reflection data and masking rules. `compileCommon` links the
endpoint to its `*Vatel` (`e.v`: shared config, base context) and resolves per-endpoint
defaults (`Timeout`, `LogOptions`, `jm`, a copy of the middleware set). The factory is called once at compile time to
inspect the controller, then once per request.

### Handler interfaces and carriers

```go
type Handler interface   { Handle(Context) error }
type Paramer interface   { Param() any }       // path params, fields tagged `param:"name"`
type Querier interface   { QueryInput() any }  // query string, every method
type Inputer interface   { Input() any }       // GET/DELETE/HEAD/OPTIONS: query; POST/PUT/PATCH: JSON body
type Resulter interface  { Result() any }      // JSON response; nil => no body
type Validator interface { Validate() error }  // on input types; called after all decoding (HTTP and WS)
```

Carriers implement them via embedding: `PathParams[T]` (`Params`), `QueryParams[T]` (`Query`, a `Querier`,
not an `Inputer`), `Body[T]` (`In`), `Output[T]` (`Out`), `OptionalOutput[T]` (`Out *T`, nil = no body),
`Upload` (multipart, call `BindFile`), `Func(f)` (plain function as factory; the only handler allowed on a
path with `{params}` without being a `Paramer`, it reads them with `ctx.PathValue`).

### Request pipeline (`Endpoint.handler`)

```
resolve log level (endpoint LogOptions, X-Set-Log-Level header, LogSilent precedence)
→ request context (base ctx + optional timeout)
→ OnRequest → OnAuthentication → OnAuthorization middleware
→ buildHandler: Paramer → Querier → Inputer (query or body) → Validate(Querier input) → Validate(Inputer input)
→ LogEnter line → OnHandle middleware → Handle
→ success status (WithSuccessStatusCode unless ManualStatusCode) → Result serialization (+ masked log)
→ LogExit line → metrics → OnSuccessResponse (errors ignored, `runHooks`)
```

Any error goes to `writeErrorResponse`: log (unless `LogSilent` and < 500), JSON error body from
`errorResponse`, metrics, `Alarmer` for >= 500, `OnErrorResponse` middleware with `e.EndpointMeta`.

A panic anywhere in the closure (middleware, decoding, handler, serialization, hooks) is recovered by a
deferred `recoverPanic`: `Response.Reset()`, then `ErrInternalError` (500, VTL-0005; the panic value reaches
the client only if verbose) with `panic` and `stack` log fields, `Alarmer`; the metric and `OnErrorResponse`
only if `VatelContext.reported` is not set yet (a hook after the response panicked). A panic inside the
recovery is logged and dropped. WS: `serveMessage` defers `wsRecoverPanic` (error frame, connection stays);
`OnOpen`/`OnClose` callbacks run through `WebsocketVatel.runCallback` (log + Alarmer; an `OnOpen` panic closes
the connection with 1011).

### Middleware

`func(Context, EndpointMeta) error` at a `MiddlewarePos`: `OnRequest`, `OnAuthentication`,
`OnAuthorization`, `OnHandle`, `OnSuccessResponse`, `OnErrorResponse`. Global via `v.AddMiddleware`,
per-endpoint via `e.AddMiddleware` inside an `EndpointOption`; global runs first. Errors from the last two
positions are ignored.

### Authentication (`auth/`)

vatel core does not check tokens or permissions; `vatel.Perms` is metadata only on HTTP endpoints.
`auth.Config{Decoder, Authorizer, RevokeCheck, Disabled}`:
- `Middleware()` (OnAuthentication) decodes `Authorization` and sets `ctx.SetTokenPayload`; a bad token is
  stored under `authTokenError`, it does not fail public endpoints. `Debug()` payload → verbose errors.
- `RequireAuth()` / `RequirePerms(map[string]uint)` (OnAuthorization) reject with 401/403; `RequirePerms`
  appends sorted names to `Endpoint.Perms`. `Disabled` is read per request; nil `Authorizer` → 500.
Token payload is `AuthPayload` (`Perms() []byte`, `Debug() bool`); handlers type-assert `ctx.TokenPayload()`.

### Errors

`github.com/axkit/errors` v1.1.0. `errorResponse` (shared by HTTP and WS) takes the status and the client
error from the first `*errors.Error`/`*errors.ErrorTemplate` in the chain (`stderrors.As`); an error without
a status maps to 504 (`context.DeadlineExceeded`), 503 (`context.Canceled`) or 500. A chain without
`*errors.Error` sends a generic error (`ErrInternalError` for 500) unless verbose: plain error text never
reaches the client. The client gets msg/severity/code/status only; fields listed in `WithClientErrorFields` are written at the JSON root; `WithVerboseError` or a debug
token adds stack, fields and wrapped errors. `Validate()` errors are wrapped in `ErrValidationFailed` (400,
VTL-0006; an own code of the returned error is kept). WS envelope errors (bad JSON, no path: VTL-0007; unknown
path: `ErrUnknownPath`, VTL-0008) go through `Vatel.errorResponse` and are logged by `writeEnvelopeError`
(no metric).

### Logging

zerolog. `LogOption` bits: `LogSilent`, `LogEnter`, `LogExit`, `LogReqBody`, `LogReqInput`, `LogRespBody`,
`LogRespOutput`; presets `LogFull`, `LogFullOnExit`, `LogConfidential`. **The default is 0 — only errors are
logged** — unless `WithDefaultLogOption` is set; `WithLogOption` overrides per endpoint. `LogSilent` logs only
5xx and overrides every other bit, including the header. `X-Set-Log-Level` applies to one request
(ignored with `WithStaticLoggingLevel`; see `docs/backlog/log-level-header-overrides-confidential.md`).
Log keys: `requestBody`, `reqInput`, `reqQuery`, `respBody` (`result` only with `LogRespOutput` without
`LogRespBody`); `ctx.Set` values are logged too, `reqId` is one of them. `logExit` is shared by HTTP and WS.

### Masking

Struct tag `mask`: `"-"` removes the field, a name applies a function registered with
`jsonmask.RawJsonMask.AddFunc` (unregistered → `"***"`). A default `jsonmask.New()` is used when
`WithJsonMasker` is not set, so tagged fields are always masked in the log; the client response is never
masked. Rules are computed at compile time (`inputFields`, `querierMaskFields`); result rules are cached per
runtime type in `resultFieldsByType` (seeded with the compile-time `Result()` type), since `Result()` may
return another type per request (e.g. `OptionalOutput`, an `any` field); masking runs only when a type has
tags. The request body of an input type with tags is logged from the decoded input, never
raw (gjson paths are case-sensitive and see only the first duplicate key). A masked param that fails to
parse gives an error without the parse error (it quotes the value). Path/query params: `mask:"-"` omits, any other value logs `"***"`. Path params are
logged only by `decodeParams`; their user-value keys (`pathParamKeys`) are skipped elsewhere.

### Request context

`Vatel` owns `baseCtx` (cancelled by `Shutdown`, see Core types). `ctx.Context()` is the base context or
`context.WithTimeout(base, Timeout)` (`WithTimeout` / `WithDefaultTimeout`), cancelled at the end of the
request. Timeouts do not interrupt handlers by themselves. fasthttp `RequestCtx.Done()` is not used.
`NewContext(fctx)` (public, for tests) uses `context.Background()`. The returned context is a
`valueContext` (embedded in `VatelContext`) wrapping that parent: `Value(key)` returns the fasthttp user
value (`Set`, `SetValue` with any key, path params, `reqId`) at call time, then the parent's value — v0.14
code passed `RequestCtx` (whose `Value` = `UserValue`) as a `context.Context`. `VatelContext.mu` guards
user-value writes and `Value` reads (other goroutines); `end()` (deferred in `Endpoint.handler`, runs after
panic recovery) detaches it so a kept context never reads the reused `RequestCtx`. Non-string keys are not
visited by `VisitUserValues`, so they are not logged.

### WebSocket

`NewWebsocketVatel(WithLogger, WithTokenDecoder, WithWsAuthFunc)` + `vatel.WithWebsocketSupport(wv)`
(`WithLogger` replaces the `BuildHandlers` logger for WS endpoints); the
app routes an HTTP path to `wv.Upgrade`. Messages are `{"path": ..., "data": ...}`; `vatel.WS(path, ctrl)`
endpoints use `Inputer` (+ `Validator`)/`Resulter` (no middleware, no Paramer/Querier). WS `Perms` require `WithWsAuthFunc`.
Built-in `updateAccessToken` endpoint calls `OnTokenUpdate`, then stores the payload per connection (`wsConns`:
one table of live connections and payloads); a callback error rejects the token. WS handlers have no Go
context. `e.wsWriteErrorResponse` uses the HTTP `errorResponse` (status mapping, `LogSilent`, metrics,
`Alarmer`). Library: `github.com/regorov/websocket` v0.1.4 — `Conn.CloseDetail` is safe from any goroutine,
`Write` after a close returns `websocket.ErrConnClosed`, but on a live connection `Write` blocks while the
128-frame queue is full (backpressure). So `onOpen` sets `Conn.WriteTimeout` (`WithWsWriteTimeout`, default
`DefaultWsWriteTimeout` 10 s), and `closeConn` sets a 1 s deadline on the net.Conn before `CloseDetail` (it
can block on a full queue); `closeAll` closes each connection in its own goroutine. `Vatel.Shutdown` waits
(within ctx) for `wsConns.drained`; `WebsocketVatel.Shutdown` does not wait. A `TokenDecoder` returning
`(nil, nil)` is a bad token (here and in `auth`).

### DI interfaces (`vatel.go`)

`TokenDecoder`, `Tokener`, `AuthPayload`, `RevokeTokenChecker`, `MetricReporter`
(`ReportMetric(method, pathTemplate, status, durSeconds, size)`), `JsonMasker`, `Alarmer`; `auth.Authorizer`.

## Conventions

- Tests: pipeline tests call `run(v, GET(...), fctx)` (`handler_test.go`; mocks in `testutil_test.go`); tests that
  depend on router user values (path params, logging) use `router.New()` + `BuildHandlers` + `mux.Handler`
  (see `logging_test.go`, `auth/auth_test.go`). Logs are checked with `zerolog.New(&buf)`. Server/shutdown
  tests use `v.serve` on a real TCP listener (`startServer`; `startServerOn` also runs a `fasthttputil`
  in-memory one) in `shutdown_test.go` — fasthttp wraps only TCP connections for `MaxConnsPerIP`; WS end-to-end tests use a
  websocket client over it (`ws_integration_test.go`). Auth is tested only through the real `auth` package.
  Tests are grouped by area, not by origin: pipeline in `handler_test.go`, logging/masking in `logging_test.go`,
  decoding/compile/BuildHandlers in `endpoint_test.go`, WS in `ws_vatel_test.go`.
- Every behaviour change visible to users goes into `MIGRATION.md`.
