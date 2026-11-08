# Migrating from v0.14 to v1.25

v1.25.0 is a rewrite of the endpoint API. Most application code that registers endpoints,
reads the token payload or adds middleware has to change. Controllers (`Handle`, `Input`,
`Param`, `Result`) keep working as before.

Version notes:

- v1.24.2 was published by mistake and is retracted (`retract` in `go.mod`). Use v1.25.0
  or later.
- `github.com/axkit/errors` v1.1.0 is required (see [section 3](#3-upgrading-from-v013v014-and-axkiterrors-v0x--v10x)).
- `github.com/axkit/bitset` is a regular dependency, v1.0.4 (no `replace` directive).

Contents:

1. [API mapping](#1-api-mapping)
2. [Behaviour changes](#2-behaviour-changes)
3. [Upgrading from v0.13/v0.14 and axkit/errors v0.x / v1.0.x](#3-upgrading-from-v013v014-and-axkiterrors-v0x--v10x)

## 1. API mapping

### Endpoint registration

| v0.14 | v1.25 |
|---|---|
| `vatel.Endpoint{Method: "GET", Path: "/x", Controller: c}` | `vatel.GET("/x", c, opts...)`; also `POST`, `PUT`, `PATCH`, `DELETE`, `HEAD`, `OPTIONS`, `WS`. The struct literal no longer compiles: `Method`, `Path`, `Perms`, `LogOptions` moved to the embedded `EndpointMeta` (`vatel.Endpoint{EndpointMeta: vatel.EndpointMeta{Method: "GET", Path: "/x"}, Controller: c}`). Field access `e.Method` still works. |
| `Endpoint{..., WebsocketController: c}` with `Method: "WS"` | `vatel.WS("path", c, opts...)` |
| `Perms: []string{"orders:read"}` (checked by vatel) | `cfg.RequirePerms(map[string]uint{"orders:read": bit})` from `vatel/auth`. `vatel.Perms(...)` still exists but is **metadata only** on HTTP endpoints: it does not protect anything. |
| `LogOptions: lo` | `vatel.WithLogOption(lo)` (the field still exists) |
| `Compress: true` | `vatel.WithCompress()` |
| `SuccessStatusCode: 201` | `vatel.WithSuccessStatusCode(201)` (now applied, see below) |
| `ManualStatusCode: true` | `vatel.WithManualStatusCode()` |
| `ResponseContentType: ct` | `vatel.WithContentType(ct)` |
| `NoInputLog`, `NoResultLog` | removed: use a `LogOption` without the body/input/output bits (e.g. `LogConfidential`) or `mask` tags |
| `LanguageLabel` | removed |
| `NewEndpoint(method, path, perms, c)` | unchanged signature; `perms` is metadata only |
| `type X []vatel.Endpoint` + `Endpoints()` method | still works; `vatel.Endpoints{...}` is a ready-made `Endpointer` |
| — | new: `vatel.Func(func(ctx vatel.Context) error)` for stateless handlers |

### Controllers

| v0.14 | v1.25 |
|---|---|
| `Param() interface{}` | unchanged (`Paramer`); or embed `vatel.PathParams[T]` (`c.Params`) |
| `Input() interface{}` | unchanged (`Inputer`): GET/DELETE/HEAD/OPTIONS from query, POST/PUT/PATCH from body; or embed `vatel.Body[T]` (`c.In`) |
| — | new: `QueryInput() interface{}` (`Querier`), query string for every method; embed `vatel.QueryParams[T]` (`c.Query`) |
| `Result() interface{}` | unchanged (`Resulter`); or embed `vatel.Output[T]` / `vatel.OptionalOutput[T]` (`c.Out`) |
| — | new: `Validate() error` (`Validator`) on input types |
| — | new: `vatel.Upload` for multipart uploads |

### Vatel configuration and authentication

| v0.14 | v1.25 |
|---|---|
| `v.SetTokenDecoder(td)` | `auth.Config{Decoder: td}` + `v.AddMiddleware(vatel.OnAuthentication, cfg.Middleware())` |
| `v.SetAuthorizer(a)` | `auth.Config{Authorizer: a}`; the interface moved to `auth.Authorizer` (same method) |
| `v.SetRevokeTokenChecker(rtc)` | `auth.Config{RevokeCheck: rtc}` |
| `v.SetPermissionManager(pm)`, `PermissionManager` | removed; `RequirePerms` takes name → bit position directly (helper below) |
| `v.DisableAuthorizer()` | `cfg.Disabled = true` |
| `v.SetRequestDebugger(rd)`, `RequestDebugger` | removed; a payload whose `Debug()` returns true enables verbose errors (`auth.Middleware`), or call `ctx.SetVerboseError(true)` in middleware |
| `TokenPayloader` (`User`, `Login`, `Role`, `Perms`, `Extra`, `Debug`) | `AuthPayload` (`Perms() []byte`, `Debug() bool`); your claims type adds whatever else it needs |
| `Tokener.ApplicationPayload() TokenPayloader` | `Tokener.ApplicationPayload() AuthPayload` |
| `Dater`, `Logger` interfaces | removed (unused) |
| `v.BuildHandlers(mux, l)`, `v.MustBuildHandlers(mux, l)` | unchanged signatures |
| — | new: `v.ListenAndServe(addr, mux, l)`, `v.Shutdown(ctx)` |
| — | new options: `WithClientErrorFields`, `WithDefaultTimeout` |

Replacing `PermissionManager`:

```go
func requirePerms(cfg *auth.Config, pm PermissionManager, names ...string) vatel.EndpointOption {
	m := make(map[string]uint, len(names))
	for _, n := range names {
		bit, ok := pm.PermissionBitPos(n)
		if !ok {
			panic("unknown permission " + n) // v0.14 returned this from BuildHandlers
		}
		m[n] = bit
	}
	return cfg.RequirePerms(m)
}
```

Endpoints that need a valid token but no permission: `cfg.RequireAuth()`.

### Context

| v0.14 | v1.25 |
|---|---|
| `ctx.TokenPayload() TokenPayloader` | `ctx.TokenPayload() any`: type-assert, `ctx.TokenPayload().(*Claims)` |
| `ctx.SetTokenPayload(TokenPayloader)` | `ctx.SetTokenPayload(any)` |
| `ctx.RequestCtx() *fasthttp.RequestCtx` | removed. Use `Header`, `RequestBody`, `PathValue`, `QueryValue`, `FormFile`, `FormValue`, `RemoteAddr` (real IP), `RequestTime`, `SetHeader`; the request ID is `ctx.Get("reqId")` with `WithRequestID()` |
| `VatelContext.Log`, `LogValues` | removed; values stored with `ctx.Set` are written to the request log |
| — | new: `ctx.Context()` (Go context), `VerboseError`/`SetVerboseError` |
| `WebsocketContext.TokenPayload() TokenPayloader` | `TokenPayload() any`, `SetTokenPayload(any)` |

### Middleware

Signature: `func(vatel.Context) error` → `func(vatel.Context, vatel.EndpointMeta) error`.

| v0.14 position | v1.25 position |
|---|---|
| `BeforeAuthorization` | `OnRequest` (before the token is decoded) |
| — | `OnAuthentication` (token decoding: `auth.Config.Middleware`) |
| — | `OnAuthorization` (access checks, before input decoding) |
| `AfterAuthorization` | `OnHandle` (after input decoding, before `Handle`, as in v0.14) |
| `OnSuccessResponse` | `OnSuccessResponse` (its error is now ignored) |
| `OnErrorResponse` (adding one panicked: index out of range) | `OnErrorResponse` (works; its error is ignored) |

Per-endpoint middleware: `e.AddMiddleware(pos, f...)` inside an `EndpointOption`.

### WebSocket

| v0.14 | v1.25 |
|---|---|
| WS `Perms` checked with the `Vatel` authorizer and permission manager | `vatel.WithWsAuthFunc(func(ctx, path, perms) error)`; required for WS endpoints with `Perms` |
| token update callback without a setter | `wv.OnTokenUpdate(func(cid uint64, tp vatel.AuthPayload) error)` |
| `NewWebsocketVatel`, `WithLogger`, `WithTokenDecoder`, `OnOpen`, `OnClose`, `Upgrade`, `UpgradeWithID` | unchanged |
| — | new: `wv.Shutdown()` (called by `v.Shutdown`) |

## 2. Behaviour changes

### Registration and build

- New registration API (`vatel.GET/...` + `EndpointOption`), carriers, positional
  middleware, authorization in `vatel/auth` (section 1).
- `Endpoint.SuccessStatusCode` (`WithSuccessStatusCode`) is now applied; in v0.14 it was
  ignored. Metrics receive the real response status (v0.14 always reported 200 for a
  successful request).
- A second `BuildHandlers` returns an error, also after a failed build.
  `ListenAndServe` after `BuildHandlers` does not register the routes again. WS
  registration errors are returned, not raised as a panic.
- `BuildHandlers`: a WS endpoint without `WithWebsocketSupport` and a duplicate WS path
  return an error that names the endpoint path (was: panic; `MustBuildHandlers` still
  panics). The duplicate WS path error from `WebsocketVatel.RegisterEndpoint` is a plain
  error "WS endpoint <path>: already registered", not an `*errors.Error` with a `path`
  field. After a failed build the `Vatel` is unusable: a further `BuildHandlers` or
  `ListenAndServe` returns a "previous build failed" error (before, `ListenAndServe` after
  a failed `BuildHandlers` started the server with part of the routes).
- Removed: `NoInputLog`, `NoResultLog`, `LanguageLabel` (public in v0.14 too), and the
  `WithLanguageLabel` option together with the `LanguageLabel` field.
- The `?description=true` query parameter no longer returns an endpoint description page;
  the request is processed normally.

### Authentication and authorization

- A bad, expired or revoked token on a public endpoint no longer gives 401: the request is
  served as unauthenticated. `RequireAuth`/`RequirePerms` endpoints return the token error.
- A request without a token on an endpoint with `RequirePerms`/`RequireAuth` gets 401
  `VTL-0004` "authentication required" (`ErrUnauthorized`); v0.14 returned `VTL-0001`
  "header Authorization missed". `ErrForbidden` no longer carries the `user` and `role`
  fields.
- `vatel.Perms` on an HTTP endpoint is metadata only (see the `vatel.Perms` comment): the
  check is done by `auth`. Code migrated by keeping `Perms` alone is unprotected.
- `auth.RequirePerms` appends the permission names, sorted, to `Endpoint.Perms`, so
  middleware sees them in `EndpointMeta.Perms` (on a WS endpoint this requires
  `WithWsAuthFunc`, otherwise `BuildHandlers` fails). `Config.Disabled` is checked on every
  request in `RequirePerms` as well (before, only when the option was created;
  `RequireAuth` and `Middleware` already checked it per request). `Config.Authorizer == nil`
  with `RequirePerms` gives 500 `ErrInternalError` instead of a panic.

### Errors

- With axkit/errors v1.1.0 the client does not see error fields unless verbose errors are
  on. Fields safe for the client are listed with `WithClientErrorFields`.
- A middleware at `OnErrorResponse` always receives the endpoint metadata (`Method`,
  `Path`, `Perms`, `LogOptions`); before, an error from middleware at `OnRequest`,
  `OnAuthentication`, `OnAuthorization` or `OnHandle` gave it an empty `EndpointMeta`.
- An error returned by an `OnSuccessResponse` middleware is ignored; v0.14 wrote an error
  response after the successful one.
- An error without its own status (not an `*errors.Error`, or an `*errors.Error` with
  status 0) whose chain contains `context.DeadlineExceeded` gives 504,
  `context.Canceled` gives 503; an explicit status wins. An `*errors.Error` without a
  status (e.g. `errors.Wrap(err, "msg")` of a plain error) now gives 500, not 200.
- A query parameter that cannot be parsed (number, bool, `date.Date`) gives 400
  `VTL-0006` with field `param`; v0.14 returned the raw parse error, i.e. 500.

### Input decoding

- A request body on DELETE is not supported; use the query string (`QueryParams`).
- New: `WithOptionalBody()` (field `Endpoint.OptionalBody`) for POST/PUT/PATCH. An empty
  or whitespace-only body is accepted, the input stays zero and `Validate()` is not called.
  On GET/DELETE it is a `BuildHandlers` error. Without the option a whitespace-only body
  now gives 400 "empty request body" (before: 400 "not valid JSON").
- New: the `Querier` interface (`QueryInput() any`). `QueryParams[T]` implements
  `QueryInput()` instead of `Input()`: code that called `Input()` on `QueryParams` must be
  changed, `QueryParams` is no longer an `Inputer`. The query string is decoded for every
  method, POST/PUT/PATCH included, so `QueryParams` + `Body` in one controller receive both
  the query and the body (before, the ambiguous `Input()` disabled input decoding
  altogether). The plain `Inputer` is unchanged: GET/DELETE from the query, POST/PUT/PATCH
  from the body. `Validate()` is called after all decoding: first on the `Querier` input,
  then on the `Inputer` input, including the query input of an `Inputer` on GET/DELETE; the
  first error gives 400. In the log the `Querier` input is written under `reqQuery`, the
  `Inputer` input under `reqInput`.
- New: builders `vatel.HEAD` and `vatel.OPTIONS` (before, these methods made
  `BuildHandlers` fail with "unknown HTTP method"). An `Inputer` on HEAD/OPTIONS is decoded
  from the query, as on GET; `WithOptionalBody()` on them is a `BuildHandlers` error. The
  fasthttp server drops the response body of HEAD. CORS preflight: `mux.GlobalOPTIONS`
  (called only for registered paths, an unknown path gets 404); an explicit `vatel.OPTIONS`
  on a path replaces `GlobalOPTIONS`; `RequireAuth`/`RequirePerms` on OPTIONS breaks the
  preflight.

### Responses

- A `Resulter` whose `Result()` returns nil or a nil pointer writes no body; v0.14 wrote
  `null`. Use `OptionalOutput[T]` for responses with an optional body.

### Logging

- `X-Set-Log-Level` applies to one request. In v0.14 it was stored on the endpoint for all
  later requests, and the request carrying it was not processed (the response was "new
  logging rules are accepted").
- `reqId` is logged only with `WithRequestID()`. v0.14 always logged it for HTTP endpoints,
  ignoring the option.
- `LogSilent` now really silences the log: nothing is written for the endpoint (HTTP and
  WS), including 4xx errors from middleware, decoding and the handler; only 5xx errors are
  written (before, `LogSilent` disabled nothing and errors were always logged). `LogSilent`
  overrides the other bits; the `X-Set-Log-Level` header does not cancel `LogSilent` of an
  endpoint, and `silent` in the header silences the request.
- Fields with a `mask` tag are always masked: without `WithJsonMasker`, `jsonmask.New()` is
  used (an unregistered function name gives `"***"`). Masked keys: `requestBody`,
  `reqInput`, `reqQuery`, `respBody`, `result`, including elements of arrays (`Body[[]T]`,
  `Output[[]T]`) and `OptionalOutput[T]`. The response sent to the client is not masked.
  The keys `maskedRequestBody`/`maskedRespBody` are gone: the masked body is written under
  the usual `requestBody`/`respBody`. `result` is written as the serialized response (the
  same JSON the client received, after masking), not through `Interface`. When masking
  fails only the error is logged (`requestBodyMaskingFailed`, `reqInputMaskingFailed`,
  `respMaskingFailed`), without the unmasked value (before, the request body was logged in
  the clear). When `json.Marshal` of the response fails, the response object is not logged
  (before, it was logged as `result`). Types without `mask` tags are logged as before.
- Path and query parameters with a `mask` tag are masked: `mask:"-"` is not logged, any
  other non-empty `mask` logs `"***"` (jsonmask functions are not applied to parameters). A
  `mask` tag on a nested struct of a query input applies to its fields without their own
  tag. Values in the input struct are not changed. Path parameters are logged only by the
  `Paramer` decoding (the key is no longer duplicated in the log; a path parameter missing
  from the `Paramer` struct is no longer logged); user values named like the `{name}`
  parameters of the path template are not logged. With `LogEnter|LogExit` path parameters
  appear only in the enter line (before, they were repeated in the exit line through user
  values).

### jsonmask

- `Fields` no longer panics (nil, scalars and maps give empty rules; new `Fields.HasRules()`),
  supports recursive types and a slice or array root (`*[]T`, `[]*T`), and visits `*Struct`,
  `[]Struct`, `[]*Struct`. A field with a non-empty `mask` tag is a leaf: a struct value
  (`time.Time`, a nested struct, a pointer, a slice) is masked or removed as a whole. A tag
  on an embedded struct without a json name applies to each of its promoted fields. An
  unregistered function name now gives `"***"` (before, the value stayed in the clear). A
  missing field (`omitempty`) is not added. Maps and interface values are not visited.

### Request context

- New: `ctx.Context()` is a child of the `Vatel` base context (before:
  `context.Background()`). `Shutdown` cancels it, after waiting for in-flight requests (up
  to the deadline of the `ctx` passed to `Shutdown`), and at once when there is no server
  started by `ListenAndServe`; a repeated `Shutdown` is safe. There is no cancellation on
  client disconnect (fasthttp does not detect it). Timeout: `vatel.WithTimeout(d)` (field
  `Endpoint.Timeout`) per endpoint and `vatel.WithDefaultTimeout(d)` on `Vatel`; a context
  with a timeout is also cancelled at the end of the request. The timeout does not
  interrupt the handler by itself, only code that listens to the context. The 504/503
  mapping is described under [Errors](#errors). `NewContext(fctx)` still uses
  `context.Background()`.

### WebSocket

- An error decoding the `data` field of a message into the `Inputer` input gives
  `ErrInvalidRequestBody` (400, code `VTL-0007`, message "ws message data is not valid
  JSON"); before, the unwrapped `json.Unmarshal` error with status 500 (in the log, the
  metrics and `Alarm`).
- An error without a status (not an `*errors.Error`, or an `*errors.Error` with status 0)
  counts as 500 in the log, metrics and `Alarm`, as in HTTP (`context.DeadlineExceeded` →
  504, `context.Canceled` → 503); before, an `*errors.Error` without a status was reported
  to metrics with status 0 and did not trigger `Alarm`.
- New: `WebsocketVatel.OnTokenUpdate(f func(cid uint64, tp AuthPayload) error)` is called
  from `UpdateAccessToken` after the payload is stored; the error of `f` is returned from
  `UpdateAccessToken` (and sent to the client of the `updateAccessToken` endpoint).

### Module

- Version v1.24.2 is retracted (`retract` in `go.mod`; it was published by mistake, move to
  v1.25.0 or later). `github.com/axkit/bitset` is a regular v1.0.4 dependency (no
  `replace`).

## 3. Upgrading from v0.13/v0.14 and axkit/errors v0.x / v1.0.x

vatel v1.25 requires `github.com/axkit/errors` v1.1.0. All major versions of that module
share one import path, so the whole application moves to v1.1.0 together with vatel.

- vatel v0.14 used errors v1.0.x. The API is compatible, but serialization changed: errors
  v1.0.x wrote error fields into every JSON, so the client saw all fields set with `Set`.
  With v1.1.0 and verbose errors off the client gets only `msg`, `severity`, `code` and
  `statusCode`. List fields the client needs (e.g. `reason`) with
  `vatel.WithClientErrorFields("reason")`; they are written at the root of the error JSON.
  Clients that read `fields.<name>` must read `<name>` instead.
- vatel v0.13 used errors v0.2.x (`CatchedError`). That API no longer exists; common
  replacements:

  | errors v0.2.x | errors v1.1.0 |
  |---|---|
  | `errors.New("msg").StatusCode(404)` at the call site | declare `var ErrX = errors.Template("msg").StatusCode(404)`, return `ErrX.New()` |
  | `errors.Catch(err)...`, `errors.Wrap(err, ce)` | `ErrX.Wrap(err)` or `errors.Wrap(err, "msg")` |
  | `.Capture()`, `.Raise()` | `ErrX.New()` (captures the stack) |
  | `.Medium()`, `.Critical()`, `errors.NewCritical(...)` | `.Severity(errors.Medium)`, `.Severity(errors.Critical)` |
  | `.Protect()` | `.Protected(true)` |
  | `.SetPairs(...)`, `.SetVals(...)`, `.SetStrs(...)` | `.Set(key, value)` |
  | `ce.Alarm(a)` | `errors.SetAlarmer(a)` once, then `err.Alarm()`; vatel calls its own `Alarmer` (`WithAlarmer`) for 5xx |

  vatel recognises the status code of an `*errors.Error` from v1.1.0 only; any other error
  type gives 500 (or 504/503 for context errors).
