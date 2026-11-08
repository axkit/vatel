# vatel

[![GoDoc](https://pkg.go.dev/badge/github.com/axkit/vatel)](https://pkg.go.dev/github.com/axkit/vatel)

vatel is a REST framework built on [fasthttp](https://github.com/valyala/fasthttp) and
[fasthttp/router](https://github.com/fasthttp/router). An endpoint is a method, a path
and a controller factory; vatel decodes path parameters, query string and JSON body into
the controller, calls it, serializes the result to JSON and handles errors, logging
(with masking of sensitive fields), metrics, authentication and authorization.

```bash
go get github.com/axkit/vatel@latest
```

Requires Go 1.24. Errors are built with [github.com/axkit/errors](https://github.com/axkit/errors) v1.1.0+.
Upgrading from v0.14? See [MIGRATION.md](MIGRATION.md).

## Contents

- [Quick start](#quick-start)
- [Endpoints](#endpoints)
- [Controllers and carriers](#controllers-and-carriers)
- [Input decoding and validation](#input-decoding-and-validation)
- [Middleware](#middleware)
- [Authentication and authorization (`vatel/auth`)](#authentication-and-authorization-vatelauth)
- [Errors](#errors)
- [Logging](#logging)
- [Masking sensitive data](#masking-sensitive-data)
- [Request context, timeouts and shutdown](#request-context-timeouts-and-shutdown)
- [HEAD, OPTIONS and CORS](#head-options-and-cors)
- [WebSocket](#websocket)
- [Other options](#other-options)

## Quick start

A complete runnable application is in [`example/`](example/) (`ADDR=:18080 go run ./example`).
The minimal version:

```go
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/axkit/errors"
	"github.com/axkit/vatel"
	"github.com/axkit/vatel/auth"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
)

type Order struct {
	ID      int    `json:"id"`
	Product string `json:"product"`
}

var errOrderNotFound = errors.Template("order not found").Code("APP-0001").StatusCode(404)

// GET /orders/{id}
type GetOrder struct {
	vatel.PathParams[struct {
		ID int `param:"id"`
	}]
	vatel.Output[Order]
}

func (c *GetOrder) Handle(ctx vatel.Context) error {
	if c.Params.ID != 1 {
		return errOrderNotFound.New().Set("id", c.Params.ID)
	}
	c.Out = Order{ID: 1, Product: "Widget"}
	return nil
}

// POST /orders
type CreateOrderInput struct {
	Product string `json:"product"`
}

// Validate is called by vatel after decoding, before Handle.
func (in *CreateOrderInput) Validate() error {
	if in.Product == "" {
		return vatel.ErrValidationFailed.New().Set("reason", "product is required")
	}
	return nil
}

type CreateOrder struct {
	vatel.Body[CreateOrderInput]
	vatel.Output[Order]
}

func (c *CreateOrder) Handle(ctx vatel.Context) error {
	c.Out = Order{ID: 2, Product: c.In.Product}
	return nil
}

func OrderEndpoints(cfg *auth.Config) vatel.Endpoints {
	read := cfg.RequirePerms(map[string]uint{"orders:read": 0}) // name -> bit position
	write := cfg.RequirePerms(map[string]uint{"orders:write": 1})

	return vatel.Endpoints{
		vatel.GET("/orders/{id}", func() vatel.Handler { return &GetOrder{} }, read),
		vatel.POST("/orders", func() vatel.Handler { return &CreateOrder{} }, write,
			vatel.WithSuccessStatusCode(201)),
		vatel.GET("/healthcheck", vatel.Func(func(ctx vatel.Context) error {
			ctx.SetStatusCode(204)
			return nil
		})),
	}
}

func main() {
	log := zerolog.New(os.Stdout).With().Timestamp().Logger()

	authCfg := &auth.Config{
		Decoder:    &JWTDecoder{},       // your vatel.TokenDecoder
		Authorizer: &BitsetAuthorizer{}, // your auth.Authorizer
	}

	v := vatel.NewVatel(
		vatel.WithDefaultLogOption(vatel.LogFullOnExit),
		vatel.WithRequestID(),
		vatel.WithClientErrorFields("reason"),
	)
	v.AddMiddleware(vatel.OnAuthentication, authCfg.Middleware())
	v.Add(OrderEndpoints(authCfg))

	mux := router.New()
	if err := v.BuildHandlers(mux, &log); err != nil {
		log.Fatal().Err(err).Msg("build handlers")
	}

	go func() {
		if err := v.ListenAndServe(":8080", mux, &log); err != nil {
			log.Fatal().Err(err).Msg("server stopped")
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := v.Shutdown(ctx); err != nil {
		log.Error().Err(err).Msg("shutdown")
	}
}
```

`JWTDecoder` and `BitsetAuthorizer` are application types; see
[`example/claims.go`](example/claims.go) for a sample implementation.

Responses:

```
GET /orders/1   (token with orders:read)  -> 200 {"id":1,"product":"Widget"}
GET /orders/5   (token with orders:read)  -> 404 {"msg":"order not found","severity":"unknown","code":"APP-0001","statusCode":404}
GET /orders/1   (no token)                -> 401 {"msg":"authentication required","severity":"medium","code":"VTL-0004","statusCode":401}
POST /orders    {"product":""}            -> 400 {"msg":"validation failed","severity":"tiny","code":"VTL-0006","statusCode":400,"reason":"product is required"}
```

`GET /` is registered automatically and returns an HTML list of all endpoints.

## Endpoints

An endpoint is created with a builder function: method, path template, controller factory
and options.

```go
vatel.GET(path, ctrl, opts...)     // also POST, PUT, PATCH, DELETE, HEAD, OPTIONS
vatel.WS(path, wsCtrl, opts...)    // WebSocket message endpoint, see below
```

`ctrl` is a `func() vatel.Handler`. It is called once per request, so each request gets
its own controller value, and once more during `BuildHandlers` to inspect the controller
type. Keep it cheap and free of side effects. `vatel.Func(f)` turns a plain
`func(vatel.Context) error` into a factory.

Path templates use fasthttp/router syntax: `/orders/{id}`, `/files/{path:*}`,
`/items/{id:[0-9]+}`.

Group endpoints with `vatel.Endpoints` (a `[]Endpoint` that implements `Endpointer`) or
with your own type that has an `Endpoints() []vatel.Endpoint` method, and register them
with `v.Add(...)`.

### Endpoint options

| Option | Effect |
|---|---|
| `vatel.Perms(names...)` | Appends permission names to `EndpointMeta.Perms`. On HTTP endpoints this is **metadata only**; use `auth.RequirePerms` to enforce permissions. On WS endpoints non-empty `Perms` require `WithWsAuthFunc`. |
| `vatel.WithLogOption(lo)` | Log verbosity of the endpoint (see [Logging](#logging)). |
| `vatel.WithSuccessStatusCode(code)` | Status of a successful response (default 200). Overrides a status set by the handler. |
| `vatel.WithManualStatusCode()` | vatel does not set the success status; the handler calls `ctx.SetStatusCode`. |
| `vatel.WithContentType(ct)` | Response `Content-Type` (default `application/json; charset=utf-8`). |
| `vatel.WithCompress()` | Wraps the handler in `fasthttp.CompressHandler`. |
| `vatel.WithOptionalBody()` | POST/PUT/PATCH: an empty body is accepted (see below). |
| `vatel.WithTimeout(d)` | Timeout of the request context (see [Request context](#request-context-timeouts-and-shutdown)). |
| `cfg.RequireAuth()`, `cfg.RequirePerms(...)` | From `vatel/auth`. |

An `EndpointOption` is a `func(*vatel.Endpoint)`, so you can write your own; it may add
per-endpoint middleware with `e.AddMiddleware(pos, f)`.

### Building handlers

`v.BuildHandlers(mux, &log)` validates every endpoint and registers it with the router.
It returns an error for, among others: a path with `{params}` and a controller that is not
a `Paramer` (or the reverse), `WithOptionalBody` on GET/DELETE/HEAD/OPTIONS, an unknown
method, a WS endpoint without `WithWebsocketSupport`, a duplicate WS path, WS `Perms`
without `WithWsAuthFunc`. `MustBuildHandlers` panics instead.

`BuildHandlers` may be called once. Registration is not transactional: after a failed
build the router may hold part of the routes, so the `Vatel` is unusable; further
`BuildHandlers`/`ListenAndServe` calls return a "previous build failed" error. Create a new
`Vatel` and router.

`v.ListenAndServe(addr, mux, &log)` builds the handlers if that has not been done yet and
starts a `fasthttp.Server`. You can also serve `mux.Handler` with your own server; then
`Shutdown` only cancels request contexts and closes WebSocket connections.

## Controllers and carriers

A controller implements `Handle(vatel.Context) error`. Capability interfaces tell vatel
what to decode and what to send back:

| Interface | Method | Purpose |
|---|---|---|
| `Paramer` | `Param() interface{}` | Pointer to a struct with `param:"name"` fields filled from path parameters. |
| `Querier` | `QueryInput() interface{}` | Pointer to a struct filled from the URL query string, for every HTTP method. |
| `Inputer` | `Input() interface{}` | GET/DELETE/HEAD/OPTIONS: filled from the query string; POST/PUT/PATCH: from the JSON body. |
| `Resulter` | `Result() interface{}` | Value serialized to the JSON response. `nil` (or a nil pointer) means no body. |
| `Validator` | `Validate() error` | Implemented by an input type; called after decoding. |

The returned pointers must point into the controller (not to a local variable).

Instead of writing these methods, embed the generic carriers:

| Carrier | Field | Implements |
|---|---|---|
| `vatel.PathParams[T]` | `Params T` | `Paramer` |
| `vatel.QueryParams[T]` | `Query T` | `Querier` |
| `vatel.Body[T]` | `In T` | `Inputer` |
| `vatel.Output[T]` | `Out T` | `Resulter` (always writes a body) |
| `vatel.OptionalOutput[T]` | `Out *T` | `Resulter`; leave `Out` nil for no body (204, 304) |
| `vatel.Upload` | `Key`, `Header` | multipart upload helper, call `BindFile(ctx)` in `Handle` |

```go
// POST /customers/{customerId}/orders/search?limit=50
type SearchFilter struct {
	Limit int `param:"limit"`
}

type SearchOrders struct {
	vatel.PathParams[struct {
		CustomerID int `param:"customerId"`
	}]
	vatel.QueryParams[SearchFilter]
	vatel.Body[struct {
		Products []string `json:"products"`
	}]
	vatel.Output[[]Order]
}

func (c *SearchOrders) Handle(ctx vatel.Context) error {
	// c.Params.CustomerID, c.Query.Limit, c.In.Products
	return nil
}
```

File upload:

```go
type ImportController struct {
	vatel.Upload
	vatel.Output[ImportResult]
}

func (c *ImportController) Handle(ctx vatel.Context) error {
	if err := c.BindFile(ctx); err != nil {
		return err
	}
	f, err := c.Open() // or c.SaveTo(ctx, path), c.StreamTo(w)
	if err != nil {
		return err
	}
	defer f.Close()
	// ...
	return nil
}

vatel.POST("/import", func() vatel.Handler {
	return &ImportController{Upload: vatel.Upload{Key: "file"}}
})
```

### Context

`vatel.Context` gives the handler access to the request and the response without exposing
fasthttp: `Header`, `RequestBody`, `PathValue`, `QueryValue`, `FormFile`, `FormValue`,
`RemoteAddr` (real IP), `RequestTime`, `SetStatusCode`, `SetHeader`, `SetContentType`,
`BodyWriter`, `TokenPayload`, `Context` (Go context), and a per-request key-value store
`Set`/`Get`. Values stored with `Set` are written to the request log.

The token payload set by the auth middleware is type-asserted to your claims type:

```go
claims := ctx.TokenPayload().(*Claims)
```

## Input decoding and validation

Order of steps for a request: path parameters (`Paramer`), query (`Querier`), query or
body (`Inputer`), then validation: `Validate()` of the `Querier` input, then `Validate()`
of the `Inputer` input. The first error rejects the request with 400.

- Path parameters: `int*`, `uint*`, `string`, `bool`, `float*` fields; the struct is flat.
- Query: the same types, pointers to them, `github.com/axkit/date.Date`; fields of nested
  structs (embedded or untagged) are flattened. A missing variable leaves the field as is.
- A value that cannot be parsed gives 400 `VTL-0006` (`ErrValidationFailed`) with field `param`.
- Body: `encoding/json` into the `Inputer` value. An empty or whitespace-only body gives
  400 `VTL-0007` "empty request body, JSON expected"; invalid JSON gives 400 `VTL-0007`
  "request body is not valid JSON".
- A request body on DELETE is not supported (RFC 9110 leaves its semantics undefined); use
  query parameters (`QueryParams`).

**Declare `Validate()` on a named type.** An anonymous struct (`vatel.Body[struct{...}]`)
cannot have methods, so its `Validate()` never exists. The method may have a pointer
receiver; it may also set default values.

```go
type SearchFilter struct {
	Limit int `param:"limit"`
}

func (f *SearchFilter) Validate() error {
	if f.Limit == 0 {
		f.Limit = 20
	}
	if f.Limit > 100 {
		return vatel.ErrValidationFailed.New().Set("reason", "limit must be <= 100")
	}
	return nil
}
```

The client sees the message "validation failed". A plain `errors.New("...")` text from
`Validate()` reaches the client only with verbose errors; to show a reason, set an error
field and list it in `WithClientErrorFields` (see [Errors](#errors)).

### Optional request body

By default POST/PUT/PATCH with an `Inputer` require a JSON body. With
`vatel.WithOptionalBody()` an empty or whitespace-only body is accepted: decoding is
skipped, the input stays zero and **`Validate()` is not called**, so a `Validate()` that
fills in defaults does not run. A non-empty body is decoded and validated as usual.

```go
vatel.POST("/orders/{id}/ticket", func() vatel.Handler { return &CreateTicket{} },
	vatel.WithOptionalBody())
```

## Middleware

Middleware has the signature `func(vatel.Context, vatel.EndpointMeta) error` and runs at
a position in the pipeline:

| Position | When |
|---|---|
| `OnRequest` | First. Rate limiting by IP, CORS headers. |
| `OnAuthentication` | Decode the token, set the token payload (`auth.Config.Middleware`). |
| `OnAuthorization` | Check access (`auth.RequireAuth`, `auth.RequirePerms`). Runs **before** input decoding. |
| `OnHandle` | After input decoding and validation, before `Handle`. Per-user limits, audit. |
| `OnSuccessResponse` | After a successful response is written. The error is ignored. |
| `OnErrorResponse` | After an error response is written. The error is ignored. |

Global middleware is added with `v.AddMiddleware(pos, f...)`, per-endpoint middleware with
`e.AddMiddleware(pos, f...)` inside an `EndpointOption`. At each position global middleware
runs first, then per-endpoint, in the order added. An error returned at
`OnRequest`..`OnHandle` stops the request and is written as the error response.

`EndpointMeta` carries the endpoint's `Method`, `Path` (template, with URL prefix),
`Perms` and `LogOptions`; every position, including `OnErrorResponse`, receives it.

```go
func audit(ctx vatel.Context, meta vatel.EndpointMeta) error {
	if len(meta.Perms) > 0 {
		ctx.Set("perms", meta.Perms) // written to the request log
	}
	return nil
}

v.AddMiddleware(vatel.OnHandle, audit)

// Per-endpoint middleware as an option.
func RateLimit(perMinute int) vatel.EndpointOption {
	return func(e *vatel.Endpoint) {
		e.AddMiddleware(vatel.OnHandle, func(ctx vatel.Context, meta vatel.EndpointMeta) error {
			// ...
			return nil
		})
	}
}
```

## Authentication and authorization (`vatel/auth`)

vatel itself does not check tokens. The `github.com/axkit/vatel/auth` package provides
middleware for a JWT-like token with a bitset of permissions.

```go
cfg := &auth.Config{
	Decoder:     myDecoder,     // vatel.TokenDecoder: Decode([]byte) (vatel.Tokener, error)
	Authorizer:  myAuthorizer,  // auth.Authorizer: IsAllowed(requestPerms []byte, bits ...uint) (bool, error)
	RevokeCheck: myRevokeCheck, // optional vatel.RevokeTokenChecker
}

v.AddMiddleware(vatel.OnAuthentication, cfg.Middleware())

v.Add(vatel.Endpoints{
	vatel.GET("/healthcheck", healthCtrl),                                   // public
	vatel.GET("/users/me", meCtrl, cfg.RequireAuth()),                       // any valid token
	vatel.GET("/orders", listCtrl, cfg.RequirePerms(map[string]uint{"orders:read": 0})),
})
```

- `Middleware()` reads the `Authorization` header, checks revocation, decodes the token and
  stores `Tokener.ApplicationPayload()` with `ctx.SetTokenPayload`. If the payload's
  `Debug()` is true, verbose errors are enabled for the request. A missing, invalid,
  expired or revoked token does **not** fail the request here: public endpoints serve it
  as unauthenticated.
- `RequireAuth()` rejects a request without a valid token (401; the token error if the
  token was bad).
- `RequirePerms(map[string]uint)` maps permission names to bit positions. It rejects a
  request without a valid token (401) or without all bits (403 `VTL-0003`). The names are
  appended, sorted, to `EndpointMeta.Perms`. A nil `Authorizer` gives 500 instead of a panic.
- `Config.Disabled = true` turns all checks off; it is read on every request.

The payload must implement `vatel.AuthPayload` (`Perms() []byte`, `Debug() bool`); handlers
type-assert `ctx.TokenPayload()` to the concrete type. `vatel.Perms(...)` alone does not
protect an HTTP endpoint.

## Errors

Return any `error` from `Handle`, `Validate` or middleware. Errors from
[`github.com/axkit/errors`](https://github.com/axkit/errors) carry an HTTP status, a code
and a severity:

```go
var ErrOrderLocked = errors.Template("order is locked").
	Code("APP-0042").StatusCode(409).Severity(errors.Medium)

return ErrOrderLocked.New().Set("orderId", id).Set("reason", "payment in progress")
return ErrOrderLocked.Wrap(err)
```

The client receives the message, severity, code and status:

```json
{"msg":"order is locked","severity":"medium","code":"APP-0042","statusCode":409}
```

Fields (`Set`), wrapped errors and the stack are written to the server log only. To show
selected fields to the client, list them once:

```go
v := vatel.NewVatel(vatel.WithClientErrorFields("reason", "suggestedName"))
// -> {"msg":"order is locked",...,"statusCode":409,"reason":"payment in progress"}
```

The listed fields are written at the root of the error JSON. Do not list fields that may
contain internal data.

Verbose errors (stack, all fields, wrapped errors in the response) are enabled for all
requests by `vatel.WithVerboseError(true)`, or per request by a token whose payload
`Debug()` returns true (or by middleware calling `ctx.SetVerboseError(true)`). Use them in
development only.

Status of an error without its own status (a plain `error`, or an `*errors.Error` with
status 0): 504 if the chain contains `context.DeadlineExceeded`, 503 for
`context.Canceled`, 500 otherwise. An error with status 429 and a `Retry-After` field sets
the `Retry-After` header. Errors with status >= 500 are passed to the `Alarmer`
(`vatel.WithAlarmer`).

Predefined errors:

| Error | Code | Status |
|---|---|---|
| `ErrAuthorizationHeaderMissed` | VTL-0001 | 401 |
| `ErrAccessTokenRevoked` | VTL-0002 | 401 |
| `ErrForbidden` | VTL-0003 | 403 |
| `ErrUnauthorized` | VTL-0004 | 401 |
| `ErrInternalError` | VTL-0005 | 500 |
| `ErrValidationFailed` | VTL-0006 | 400 |
| `ErrInvalidRequestBody` | VTL-0007 | 400 |

## Logging

vatel logs with [zerolog](https://github.com/rs/zerolog). Each endpoint logger carries
`method`, `path` and `client` (real IP); `WithRequestID()` adds `reqId`.

What is logged is a bitmask (`LogOption`):

| Bit | Header value | Logs |
|---|---|---|
| `LogEnter` | `enter` | a "new request" line when the request arrives |
| `LogExit` | `exit` | a line when the request completes, with `dur` |
| `LogReqBody` | `req-body` | raw request body as `requestBody` |
| `LogReqInput` | `req-input` | decoded input as `reqInput` (`Inputer`) and `reqQuery` (`Querier`) |
| `LogRespBody` | `resp-body` | response body as `respBody` |
| `LogRespOutput` | `resp-out` | response as `result` |
| `LogSilent` | `silent` | nothing except 5xx errors; overrides all other bits |

Presets: `LogFull` (enter, exit, request body and input, response body), `LogFullOnExit`
(the same without enter), `LogConfidential` (exit only).

- **The default level is 0: only errors are logged.** Set a default with
  `vatel.WithDefaultLogOption(lo)` and override it per endpoint with
  `vatel.WithLogOption(lo)`.
- Errors are logged at error level with the full error (fields, wrapped errors, stack),
  the path and query parameters decoded so far and context values. With `LogSilent` only 5xx errors are logged.
- Path and query parameters are logged by name; values stored with `ctx.Set` are logged
  as well. A value stored under the key `message` replaces the exit line message.
- The `X-Set-Log-Level` request header (`X-Set-Log-Level: exit,req-body`) replaces the
  endpoint level for that one request. It cannot switch off `LogSilent` of an endpoint;
  `silent` in the header silences the request. `vatel.WithStaticLoggingLevel()` ignores
  the header. **Any client can send this header**; use `WithStaticLoggingLevel()` in
  production unless every secret in request and response types is tagged with `mask`.

## Masking sensitive data

Tag fields with `mask` and they are masked in the log. The response sent to the client is
not changed.

```go
type LoginInput struct {
	Login    string `json:"login"`
	Password string `json:"password" mask:"-"`      // removed from the log
	Card     string `json:"card"     mask:"secret"` // "***"
	Phone    string `json:"phone"    mask:"phone"`  // custom function
}
```

- `mask:"-"` removes the field; any other value names a masking function. A name not
  registered with `AddFunc` gives `"***"` (`jsonmask.DefaultMask`).
- Without `vatel.WithJsonMasker` a default `jsonmask.New()` is used, so tagged fields are
  always masked. Register functions with your own masker:

  ```go
  jm := jsonmask.New()
  jm.AddFunc("phone", func(s string) string {
  	if len(s) < 4 {
  		return jsonmask.DefaultMask
  	}
  	return strings.Repeat("*", len(s)-4) + s[len(s)-4:]
  })
  v := vatel.NewVatel(vatel.WithJsonMasker(jm))
  ```

- Masked log keys: `requestBody`, `reqInput`, `reqQuery`, `respBody`, `result`, including
  elements of arrays (`Body[[]T]`, `Output[[]T]`) and `OptionalOutput[T]`. If masking
  fails only the error is logged (`requestBodyMaskingFailed`, `reqInputMaskingFailed`,
  `respMaskingFailed`), never the unmasked value.
- Tags are honoured in nested structs, `*Struct`, `[]Struct`, `[]*Struct` and recursive
  types. A tagged field is a leaf: a tagged struct, pointer or slice (e.g. `time.Time`) is
  masked or removed as a whole. A tag on an embedded struct applies to each promoted field.
  Maps and `interface{}` values are not visited. Fields absent from the JSON
  (`omitempty`) are not added.
- **Path and query parameters**: `mask:"-"` omits the parameter from the log, any other
  non-empty `mask` logs `"***"`. Masking functions are not applied to parameters. A `mask`
  tag on a nested struct of a query input applies to its fields without their own tag.

```go
type ResetPassword struct {
	vatel.PathParams[struct {
		Token string `param:"token" mask:"secret"` // logged as "***"
	}]
	vatel.QueryParams[struct {
		Email string `param:"email" mask:"-"` // not logged
	}]
}
```

Types without `mask` tags are logged unchanged.

## Request context, timeouts and shutdown

`ctx.Context()` returns a Go context for the request; pass it to database and HTTP calls.

- It is a child of a base context owned by the `Vatel`. `v.Shutdown(ctx)` closes WebSocket
  connections, waits for in-flight requests (until `ctx` is done), then cancels the base
  context, so handlers still running get `context.Canceled`. Without a server started by
  `ListenAndServe`, the base context is cancelled at once. Calling `Shutdown` again is safe.
- `vatel.WithTimeout(d)` (endpoint) and `vatel.WithDefaultTimeout(d)` (`Vatel`, for
  endpoints without their own timeout) add a deadline. Such a context is also cancelled
  when the request ends.
- **The timeout does not interrupt the handler by itself.** Only code that listens to the
  context stops (database drivers, `http.NewRequestWithContext`, `select` on `Done()`).
- A handler error whose chain contains `context.DeadlineExceeded` gives 504,
  `context.Canceled` gives 503, unless the error carries its own status.
- fasthttp does not report a client disconnect, so the context is not cancelled when the
  client goes away.
- `vatel.NewContext(fctx)` (for tests) returns a `Context` with `context.Background()`.

```go
func (c *Report) Handle(ctx vatel.Context) error {
	rows, err := c.db.QueryContext(ctx.Context(), query)
	if err != nil {
		return err // DeadlineExceeded -> 504
	}
	defer rows.Close()
	// ...
}

vatel.GET("/reports/sales", reportCtrl, vatel.WithTimeout(5*time.Second))
```

## HEAD, OPTIONS and CORS

`vatel.HEAD` and `vatel.OPTIONS` work like `vatel.GET`: an `Inputer` is filled from the
query string, `WithOptionalBody` is not allowed. On HEAD the fasthttp server drops the
response body, so only the status and headers reach the client.

For CORS preflight use the router's `GlobalOPTIONS` handler. The router calls it only for
registered paths (an unknown path gets 404) and sets the `Allow` header before calling it.
An explicit `vatel.OPTIONS` endpoint on a path replaces `GlobalOPTIONS` for that path.
**Do not put `RequireAuth`/`RequirePerms` on an OPTIONS endpoint**: browsers send the
preflight without the `Authorization` header.

```go
mux := router.New()
mux.GlobalOPTIONS = func(fctx *fasthttp.RequestCtx) {
	h := &fctx.Response.Header
	h.Set("Access-Control-Allow-Origin", allowedOrigin(fctx.Request.Header.Peek("Origin")))
	h.Set("Access-Control-Allow-Methods", string(h.Peek("Allow")))
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	h.Set("Access-Control-Max-Age", "600")
	h.Add("Vary", "Origin")
	fctx.SetStatusCode(fasthttp.StatusNoContent)
}

// Allow-Origin on actual responses, including errors.
v.AddMiddleware(vatel.OnRequest, func(ctx vatel.Context, _ vatel.EndpointMeta) error {
	ctx.SetHeader([]byte("Access-Control-Allow-Origin"), []byte(allowedOrigin(ctx.Header("Origin"))))
	ctx.SetHeader([]byte("Vary"), []byte("Origin"))
	return nil
})
```

See [`example/cors.go`](example/cors.go).

## WebSocket

WebSocket endpoints share the endpoint model. A client sends JSON messages
`{"path": "<endpoint path>", "data": {...}}`; `data` is decoded into the handler's
`Inputer`, the `Resulter` value is sent back as JSON.

```go
type Ping struct {
	in struct {
		Seq int `json:"seq"`
	}
	out struct {
		Seq int `json:"seq"`
	}
}

func (h *Ping) Input() interface{}  { return &h.in }
func (h *Ping) Result() interface{} { return &h.out }
func (h *Ping) Handle(ctx vatel.WebsocketContext) error {
	h.out.Seq = h.in.Seq
	return nil
}

wv := vatel.NewWebsocketVatel(
	vatel.WithTokenDecoder(myDecoder),
	vatel.WithWsAuthFunc(func(ctx vatel.WebsocketContext, path string, perms []string) error {
		if len(perms) > 0 && ctx.TokenPayload() == nil {
			return vatel.ErrUnauthorized.New()
		}
		return nil // check perms against ctx.TokenPayload()
	}),
)
wv.OnTokenUpdate(func(cid uint64, tp vatel.AuthPayload) error { return nil })

v := vatel.NewVatel(vatel.WithWebsocketSupport(wv))
v.Add(vatel.Endpoints{
	vatel.WS("ping", func() vatel.WebsocketHandler { return &Ping{} }),
	vatel.WS("orders.watch", watchCtrl, vatel.Perms("orders:read")),
})

mux := router.New()
mux.GET("/ws", wv.Upgrade) // the HTTP route that upgrades to WebSocket
```

- The built-in endpoint `updateAccessToken` (`data`: `{"mid": "<uuid>", "at": "<token>"}`)
  decodes the token with `WithTokenDecoder`, stores the payload for the connection and
  calls the `OnTokenUpdate` callback; an error from the callback is sent to the client.
  The stored payload is available as `ctx.TokenPayload()` in later messages.
- `WithWsAuthFunc` is called for every message with the endpoint's `Perms`; WS endpoints
  with `Perms` require it. HTTP middleware does not run for WS messages.
- Invalid `data` gives a 400 `VTL-0007` error message. Errors are logged and reported to
  metrics and the `Alarmer` like HTTP errors.
- `OnOpen` and `OnClose` register connection callbacks. `v.Shutdown` closes all WebSocket
  connections with a "going away" status first.

## Other options

| `Vatel` option | Effect |
|---|---|
| `WithUrlPrefix("/api/v1")` | Prefix joined to every HTTP endpoint path. |
| `WithRequestID()` | Log `reqId` (fasthttp request ID); also stored in the context as `reqId`. |
| `WithVerboseError(true)` | Stack, fields and wrapped errors in error responses. |
| `WithClientErrorFields(names...)` | Error fields shown to the client. |
| `WithDefaultLogOption(lo)` | Log level of endpoints without their own. |
| `WithStaticLoggingLevel()` | Ignore the `X-Set-Log-Level` header. |
| `WithDefaultTimeout(d)` | Request context timeout of endpoints without their own. |
| `WithJsonMasker(jm)` | Masker for logged JSON (default `jsonmask.New()`). |
| `WithMetricReporter(mr)` | `ReportMetric(method, path, statusCode, durSeconds, size)` per request; `path` is the template. |
| `WithAlarmer(a)` | `Alarm(err)` for errors with status >= 500. |
| `WithWebsocketSupport(wv)` | Enables WS endpoints. |

## License

MIT, see [LICENSE](LICENSE).
