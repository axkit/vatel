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

# Tidy dependencies
go mod tidy
```

## Architecture Overview

Vatel is a REST framework built on [fasthttp](https://github.com/valyala/fasthttp) and
fasthttp/router. User docs: `README.md`; v0.14 → v1.25 changes: `MIGRATION.md`. Plans live in
`docs/plans/`, deferred work in `docs/backlog/`.

### Files

- `vatel.go` — `Vatel` (registry, `Option`s, `BuildHandlers`, `ListenAndServe`, `Shutdown`), DI interfaces.
- `options.go` — `EndpointOption`s and builders `GET/POST/PUT/PATCH/DELETE/HEAD/OPTIONS/WS`, `Endpoints`.
- `endpoint.go` — `Endpoint`, handler interfaces, request pipeline (`handler`, `buildHandler`), logging,
  error responses, `compile`, `LogOption` bits, sentinel errors (`VTL-0001`..`VTL-0007`).
- `decode.go` — path/query/body decoding with precomputed `fieldInfo`, `validate`, `Validator`.
- `carriers.go` — generic carriers `PathParams`, `QueryParams`, `Body`, `Output`, `OptionalOutput`, `Upload`, `Func`.
- `middleware.go` — `MiddlewarePos`, `EndpointMeta`.
- `context.go` — `Context` interface and `VatelContext` (wraps `fasthttp.RequestCtx`, not exposed).
- `ws_vatel.go`, `ws_context.go`, `ws_access_token_update.go` — WebSocket endpoints.
- `auth/` — JWT-style authentication/authorization middleware.
- `jsonmask/` — masking of JSON by `mask` struct tags.
- `vatel_c_toc.go` — `GET /` table of contents, registered automatically by `NewVatel`.
- `example/` — runnable app: auth, carriers, CORS, upload, graceful shutdown.

### Core types

**`Vatel`** — created by `NewVatel(opts...)`. `Add(Endpointer...)` collects endpoints;
`BuildHandlers(mux, logger)` compiles each endpoint and registers it with the router. It may be called
once; after a failed build the `Vatel` is unusable ("previous build failed"), registration is not
transactional. `ListenAndServe` builds if needed and starts `fasthttp.Server`; `Shutdown(ctx)` closes WS
connections, waits for the server, then cancels the base request context.

**`Endpoint`** — embeds `EndpointMeta` (`Method`, `Path`, `Perms`, `LogOptions`) plus public config
(`Compress`, `ManualStatusCode`, `SuccessStatusCode`, `ResponseContentType`, `OptionalBody`, `Timeout`) and
a controller factory (`Controller func() Handler` or `WebsocketController`). Built with builders
(`vatel.GET(path, ctrl, opts...)`) and `EndpointOption`s (`Perms`, `WithLogOption`, `WithCompress`,
`WithSuccessStatusCode`, `WithManualStatusCode`, `WithContentType`, `WithOptionalBody`, `WithTimeout`).
`compile()` validates config (Paramer vs `{params}` in path, `OptionalBody` only on POST/PUT/PATCH, known
method) and precomputes reflection data and masking rules. The factory is called once at compile time to
inspect the controller, then once per request.

### Handler interfaces and carriers

```go
type Handler interface  { Handle(Context) error }
type Paramer interface  { Param() interface{} }      // path params, fields tagged `param:"name"`
type Querier interface  { QueryInput() interface{} } // query string, every method
type Inputer interface  { Input() interface{} }      // GET/DELETE/HEAD/OPTIONS: query; POST/PUT/PATCH: JSON body
type Resulter interface { Result() interface{} }     // JSON response; nil => no body
type Validator interface { Validate() error }        // on input types; called after all decoding
```

Carriers implement them via embedding: `PathParams[T]` (`Params`), `QueryParams[T]` (`Query`, a `Querier`,
not an `Inputer`), `Body[T]` (`In`), `Output[T]` (`Out`), `OptionalOutput[T]` (`Out *T`, nil = no body),
`Upload` (multipart, call `BindFile`), `Func(f)` (plain function as factory).

### Request pipeline (`Endpoint.handler`)

```
resolve log level (endpoint LogOptions, X-Set-Log-Level header, LogSilent precedence)
→ request context (base ctx + optional timeout)
→ OnRequest → OnAuthentication → OnAuthorization middleware
→ buildHandler: Paramer → Querier → Inputer (query or body) → Validate(Querier input) → Validate(Inputer input)
→ LogEnter line → OnHandle middleware → Handle
→ success status (WithSuccessStatusCode unless ManualStatusCode) → Result serialization (+ masked log)
→ LogExit line → metrics → OnSuccessResponse
```

Any error goes to `writeErrorResponse`: log (unless `LogSilent` and < 500), JSON error body, metrics,
`Alarmer` for >= 500, `OnErrorResponse` middleware with `e.EndpointMeta`.

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

`github.com/axkit/errors` v1.1.0. Status/code come from `*errors.Error`; an error without a status maps to
504 (`context.DeadlineExceeded`), 503 (`context.Canceled`) or 500. The client gets msg/severity/code/status
only; fields listed in `WithClientErrorFields` are written at the JSON root; `WithVerboseError` or a debug
token adds stack, fields and wrapped errors. Validation errors are wrapped as 400.

### Logging

zerolog. `LogOption` bits: `LogSilent`, `LogEnter`, `LogExit`, `LogReqBody`, `LogReqInput`, `LogRespBody`,
`LogRespOutput`; presets `LogFull`, `LogFullOnExit`, `LogConfidential`. **The default is 0 — only errors are
logged** — unless `WithDefaultLogOption` is set; `WithLogOption` overrides per endpoint. `LogSilent` logs only
5xx and overrides every other bit, including the header. `X-Set-Log-Level` applies to one request
(ignored with `WithStaticLoggingLevel`; see `docs/backlog/log-level-header-overrides-confidential.md`).
Log keys: `requestBody`, `reqInput`, `reqQuery`, `respBody`, `result`; `ctx.Set` values are logged too.

### Masking

Struct tag `mask`: `"-"` removes the field, a name applies a function registered with
`jsonmask.RawJsonMask.AddFunc` (unregistered → `"***"`). A default `jsonmask.New()` is used when
`WithJsonMasker` is not set, so tagged fields are always masked in the log; the client response is never
masked. Rules are computed at compile time (`inputFields`, `queryMaskFields`, `resultFields`); masking runs
only when a type has tags. Path/query params: `mask:"-"` omits, any other value logs `"***"`. Path params are
logged only by `decodeParams`; their user-value keys (`pathParamKeys`) are skipped elsewhere.

### Request context

`Vatel` owns `baseCtx` (cancelled by `Shutdown`). `ctx.Context()` is the base context or
`context.WithTimeout(base, Timeout)` (`WithTimeout` / `WithDefaultTimeout`), cancelled at the end of the
request. Timeouts do not interrupt handlers by themselves. fasthttp `RequestCtx.Done()` is not used.
`NewContext(fctx)` (public, for tests) uses `context.Background()`.

### WebSocket

`NewWebsocketVatel(WithLogger, WithTokenDecoder, WithWsAuthFunc)` + `vatel.WithWebsocketSupport(wv)`; the
app routes an HTTP path to `wv.Upgrade`. Messages are `{"path": ..., "data": ...}`; `vatel.WS(path, ctrl)`
endpoints use `Inputer`/`Resulter` (no middleware, no Paramer/Querier). WS `Perms` require `WithWsAuthFunc`.
Built-in `updateAccessToken` endpoint stores the payload per connection and calls `OnTokenUpdate`.
`wsWriteErrorResponse` mirrors HTTP error handling (status mapping, `LogSilent`, metrics, `Alarmer`).

### DI interfaces (`vatel.go`)

`TokenDecoder`, `Tokener`, `AuthPayload`, `RevokeTokenChecker`, `MetricReporter`
(`ReportMetric(method, pathTemplate, status, durSeconds, size)`), `JsonMasker`, `Alarmer`; `auth.Authorizer`.

## Conventions

- Tests: pipeline tests call `run(v, GET(...), fctx)` (`handler_test.go`; mocks in `testutil_test.go`); tests that
  depend on router user values (path params, logging) use `router.New()` + `BuildHandlers` + `mux.Handler`
  (see `logging_test.go`, `auth/auth_test.go`). Logs are checked with `zerolog.New(&buf)`.
- Every behaviour change visible to users goes into `MIGRATION.md`.
