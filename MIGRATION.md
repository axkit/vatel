# Migrating from v0.14 to v1.25

v1.25.0 is a rewrite of the endpoint API. Most application code that registers endpoints,
reads the token payload or adds middleware has to change. Controllers (`Handle`, `Input`,
`Param`, `Result`) keep working as before.

Version notes:

- v1.24.2 was published by mistake and is retracted (`retract` in `go.mod`). Use v1.25.0
  or later.
- `github.com/axkit/errors` v1.1.0 is required (see [section 3](#3-upgrading-from-v013v014-and-axkiterrors-v0x--v10x)).
- `github.com/axkit/bitset` is a regular dependency, v1.0.4 (no `replace` directive).
- Go 1.26, `github.com/valyala/fasthttp` v1.77.0 and `github.com/fasthttp/router` v1.5.4
  are the new minimums (v0.14: Go 1.24, fasthttp v1.31.0, router v1.4.4); see
  [Module](#module).

Contents:

1. [API mapping](#1-api-mapping)
2. [Behaviour changes](#2-behaviour-changes)
3. [Upgrading from v0.13/v0.14 and axkit/errors v0.x / v1.0.x](#3-upgrading-from-v013v014-and-axkiterrors-v0x--v10x)
4. [Changes since the retracted v1.24.2](#4-changes-since-the-retracted-v1242)

Sections 1–3 compare v1.25 with v0.14. If you tried the retracted v1.24.2, also read
section 4.

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
| — | new: `v.ListenAndServe(addr, mux, l)`, `v.Shutdown(ctx)`, `vatel.ErrServerClosed`, `vatel.DefaultIdleTimeout` |
| — | new options: `WithClientErrorFields`, `WithDefaultTimeout`, `WithServerConfig` |

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
| custom implementations of `vatel.Context` (test mocks) | must add the new methods: `Context`, `PathValue`, `QueryValue`, `RemoteAddr`, `RequestTime`, `RequestBody`, `VerboseError`, `SetVerboseError` |
| `WebsocketContext.TokenPayload() TokenPayloader` | `TokenPayload() any`, `SetTokenPayload(any)` |

### Middleware

Signature: `func(vatel.Context) error` → `func(vatel.Context, vatel.EndpointMeta) error`.

| v0.14 position | v1.25 position |
|---|---|
| `BeforeAuthorization` | `OnRequest` (before the token is decoded) |
| — | `OnAuthentication` (token decoding: `auth.Config.Middleware`) |
| — | `OnAuthorization` (access checks, before input decoding) |
| `AfterAuthorization` | `OnHandle` (after input decoding, before `Handle`, as in v0.14) |
| `OnSuccessResponse` | `OnSuccessResponse` (its error is now ignored: the response is not changed and later middleware still runs) |
| `OnErrorResponse` (adding one panicked: index out of range) | `OnErrorResponse` (works; its error is ignored) |

Per-endpoint middleware: `e.AddMiddleware(pos, f...)` inside an `EndpointOption`.

### WebSocket

| v0.14 | v1.25 |
|---|---|
| WS `Perms` checked with the `Vatel` authorizer and permission manager | `vatel.WithWsAuthFunc(func(ctx, path, perms) error)`; required for WS endpoints with `Perms` |
| token update callback without a setter | `wv.OnTokenUpdate(func(cid uint64, tp vatel.AuthPayload) error)` |
| `NewWebsocketVatel`, `WithTokenDecoder`, `OnOpen`, `OnClose`, `Upgrade`, `UpgradeWithID` | unchanged |
| `WithLogger` (stored, but not used) | now used: WS endpoints log to it (`"layer":"ws"`); without it, to the logger passed to `BuildHandlers` |
| `AuthMessageData` | deprecated, unused |
| — | new: `wv.Shutdown()` (called by `v.Shutdown`) |

## 2. Behaviour changes

### Registration and build

- New registration API (`vatel.GET/...` + `EndpointOption`), carriers, positional
  middleware, authorization in `vatel/auth` (section 1).
- `Endpoint.SuccessStatusCode` (`WithSuccessStatusCode`) is now applied; in v0.14 it was
  ignored. Metrics receive the real response status (v0.14 always reported 200 for a
  successful request).
- A second `BuildHandlers` returns an error, also after a failed build. After a failed
  build the `Vatel` is unusable: a further `BuildHandlers` or `ListenAndServe` returns a
  "previous build failed" error.
- `BuildHandlers` returns errors where v0.14 panicked (`MustBuildHandlers` still panics):
  a WS endpoint without `WithWebsocketSupport`, a duplicate WS path (both name the path),
  a route the router rejects (a duplicate or conflicting path, your own `GET /`, which is
  taken by the built-in endpoint list), a path that does not begin with `/`, and a
  `Param()`, `QueryInput()` or GET/DELETE/HEAD/OPTIONS `Input()` that does not return a
  pointer to a struct. A nested struct of a type already being flattened in a query or
  path input (a cycle such as `Parent *Filter`) is skipped instead of recursing forever.
- `BuildHandlers` errors name the endpoint as "endpoint <METHOD> <path>: <reason>"
  (`WS` for WS endpoints).
- Middleware and endpoints added after `BuildHandlers` have no effect: it copies them.
- Removed: `NoInputLog`, `NoResultLog`, `LanguageLabel`.
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
  `WithWsAuthFunc`, otherwise `BuildHandlers` fails). `Config.Disabled` is read on every
  request; set it before the server starts serving. `Config.Authorizer == nil` with
  `RequirePerms` gives 500 `ErrInternalError`.
- `auth.Config.Decoder` receives the raw `Authorization` header value (nothing strips a
  `Bearer ` prefix, as in v0.14). A nil `Decoder` or a `RevokeCheck` error gives 500
  `VTL-0005` on `RequireAuth`/`RequirePerms` endpoints; public endpoints serve the
  request as unauthenticated.

### Errors

- With axkit/errors v1.1.0 the client does not see error fields unless verbose errors are
  on. Fields safe for the client are listed with `WithClientErrorFields`.
- **The text of a plain error no longer reaches the client.** v0.14 sent `err.Error()` of
  any error, e.g. a database driver message. Now the client gets the first
  `*errors.Error` (or `*errors.ErrorTemplate`) found in the error chain, so
  `fmt.Errorf("...: %w", vatel.ErrForbidden.New())` gives 403 with the `ErrForbidden`
  message (before: 500 with the wrapper text). An error chain without one gives a generic
  error: `"internal error"`, `VTL-0005`, 500 (or "service unavailable"/"gateway timeout"
  for 503/504). The full error is logged. With verbose errors the client gets the
  original text. The same applies to WebSocket error messages.
- A middleware at `OnErrorResponse` receives the endpoint metadata (`Method`, `Path`,
  `Perms`, `LogOptions`), whatever the pipeline position of the error.
- An error returned by an `OnSuccessResponse` middleware is ignored: the response, its
  status, the metrics and the log are not changed and later `OnSuccessResponse`
  middleware still runs. v0.14 wrote an error response after the successful one.
- An error without its own status (not an `*errors.Error`, or an `*errors.Error` with
  status 0) whose chain contains `context.DeadlineExceeded` gives 504,
  `context.Canceled` gives 503; an explicit status wins. An `*errors.Error` without a
  status (e.g. `errors.Wrap(err, "msg")` of a plain error) now gives 500, not 200.
- A query parameter that cannot be parsed (number, bool, `date.Date`) gives 400
  `VTL-0006` with field `param`; v0.14 returned the raw parse error, i.e. 500. For a path
  or query parameter with a `mask` tag the error does not wrap the parse error, which
  quotes the value.
- Path parameters support the same field types as query parameters (pointers,
  `date.Date`, nested structs); a `float32` path parameter is parsed with 32-bit
  precision.
- A `Validate()` error gives `ErrValidationFailed` (400, code `VTL-0006`, severity tiny)
  wrapping it; the code of a returned `*errors.Error`, if it has one, replaces `VTL-0006`
  and its fields are kept. Before, a plain error from `Validate()` gave 400 without a code.
- `Upload.BindFile` returns `ErrInvalidRequestBody` (400, `VTL-0007`) with field `field`
  for a request that is not a multipart form or has no such file field; before, the raw
  fasthttp error, i.e. a generic 500 and an `Alarm`.

### Input decoding

- A request body on DELETE is not supported; use the query string (`QueryParams`).
- A body (or WS `data`) of an input type with `mask` tags that cannot be decoded gives
  `ErrInvalidRequestBody` without the `encoding/json` error wrapped, since it may quote a
  value (a number that does not fit the field, a `time.Time`, a custom `UnmarshalJSON`
  error); only the fields `field`, `type` and `offset` are set. Types without `mask` tags
  keep the wrapped error.
- New: `WithOptionalBody()` (field `Endpoint.OptionalBody`) for POST/PUT/PATCH. An empty
  or whitespace-only body is accepted, the input stays zero and `Validate()` is not called.
  On GET/DELETE it is a `BuildHandlers` error. Without the option a whitespace-only body
  now gives 400 "empty request body" (before: 400 "not valid JSON").
- New: the `Querier` interface (`QueryInput() any`), implemented by `QueryParams[T]`. The
  query string is decoded for every method, POST/PUT/PATCH included, so `QueryParams` +
  `Body` in one controller receive both the query and the body. The plain `Inputer` is
  unchanged: GET/DELETE from the query, POST/PUT/PATCH from the body. `Validate()` is called after all decoding: first on the `Querier` input,
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
  `respBodyMaskingFailed`/`resultMaskingFailed`), without the unmasked value (before, the
  request body was logged in the clear).
- The request body of an input type with `mask` tags is logged under `requestBody` as the
  decoded input, marshalled and masked, not as the raw body: `encoding/json` also decodes
  keys in another letter case and the last of duplicate keys, which the masker would miss.
  Keys of the body that are not in the input type are therefore not logged, and a body
  that cannot be decoded is not logged (`requestBodyLoggingFailed`). Bodies of types
  without `mask` tags are logged compacted, as before.
- `LogRespOutput` and `LogRespBody` log the same masked JSON; with both bits set it is
  written once, under `respBody`. `LogRespOutput` alone writes `result`.
- With `WithRequestID()` and `LogEnter` or `LogExit`, `reqId` is written once per line
  (it was duplicated). A non-string `message` context value no longer panics; it is
  formatted with `fmt.Sprint`. When `json.Marshal` of the response fails, the response object is not logged
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
  on an embedded struct without a json name applies to each of its promoted fields that
  has no tag of its own. An
  unregistered function name now gives `"***"` (before, the value stayed in the clear). A
  missing field (`omitempty`) is not added. Maps and interface values are not visited.

### Request context

- New: `ctx.Context()` is a child of the `Vatel` base context. `Shutdown(ctx)` stops
  accepting connections, waits until no request is in flight, including requests whose
  headers or body are still being received, closes idle keep-alive connections (a new
  connection that has sent nothing gets about 5 s to start its request), cancels the base
  context and returns nil. When `ctx` is done first,
  it cancels the base context at once, waits up to one more second for the cancelled
  requests and returns `ctx.Err()`; requests still running then are not interrupted and
  their connections close when they finish. Without a server started by `ListenAndServe`
  it cancels at once; a repeated or concurrent `Shutdown` is safe (each call waits with
  its own `ctx`). `ListenAndServe` returns nil after
  `Shutdown` and `ErrServerClosed` when called after it. The server it starts has
  `IdleTimeout` = `DefaultIdleTimeout` (90 s) and `CloseOnShutdown`; change it with
  `WithServerConfig` (any field but `Handler`, including `MaxConnsPerIP` and `ConnState`). There is no cancellation on client disconnect (fasthttp does not
  detect it). Timeout: `vatel.WithTimeout(d)` (field
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
- Errors of the message envelope are coded and logged: a message that is not valid JSON
  gives `ErrInvalidRequestBody` (400, `VTL-0007`, "ws message is not valid JSON"; before:
  400 "bad request" without a code), a message without `path` gives `ErrInvalidRequestBody`
  ("ws message has no path"; before: 400 "bad request" without a code), an unknown `path`
  gives the new `ErrUnknownPath` (404, `VTL-0008`; before: 404 "unknown path" without a
  code). They are logged with `"method":"WS"` and the requested `path` (before: not
  logged); no metric is reported.
- A WebSocket connection opened after `Shutdown` (of `Vatel` or `WebsocketVatel`) is
  closed at once with a "going away" status, without the `OnOpen`/`OnClose` callbacks.
- New: `WebsocketVatel.OnTokenUpdate(f func(cid uint64, tp AuthPayload) error)` is called
  from `UpdateAccessToken` after the token is decoded and before the payload is stored. An
  error from `f` rejects the token: the payload is not stored (the connection keeps its
  previous one) and the error is returned from `UpdateAccessToken` (and sent to the client
  of the `updateAccessToken` endpoint). A token that cannot be decoded gives
  `ErrUnauthorized` (401).

### Metrics

- The size reported for a HEAD response is 0: the server drops its body.
- The size reported for a successful WebSocket message is the size of the reply (before:
  0).

### Module

- Version v1.24.2 is retracted (`retract` in `go.mod`; it was published by mistake, move to
  v1.25.0 or later). `github.com/axkit/bitset` is a regular v1.0.4 dependency (no
  `replace`).
- Minimum versions: Go 1.26 (required by fasthttp v1.77.0), `github.com/valyala/fasthttp`
  v1.77.0 (v0.14: v1.31.0) and `github.com/fasthttp/router` v1.5.4 (v0.14: v1.4.4).
  `Shutdown` relies on fasthttp's `ShutdownWithContext` of these versions. Go's minimal
  version selection moves your project to them when you upgrade vatel; code that uses
  fasthttp or fasthttp/router directly should be checked against their changelogs
  between these versions.

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
  type gives 500 (or 504/503 for context errors) with a generic message.

## 4. Changes since the retracted v1.24.2

v1.24.2 was published from an earlier state of this rewrite. Besides sections 1–3, code
written against it may need these changes:

- `QueryParams[T]` implements `QueryInput()` (`Querier`) instead of `Input()`; code that
  called `Input()` on `QueryParams` must call `QueryInput()`. With `QueryParams` + `Body`
  both are decoded (in v1.24.2 the ambiguous `Input()` disabled input decoding).
- The `WithLanguageLabel` option is removed.
- `ctx.Context()` is a child of the `Vatel` base context (v1.24.2: `context.Background()`).
- `auth`: `Config.Disabled` is checked on every request in `RequirePerms` too (v1.24.2:
  when the option was created). `Config.Authorizer == nil` gives 500 instead of a panic.
- `OnErrorResponse` middleware receives the endpoint metadata for errors at every
  pipeline position (v1.24.2: an empty `EndpointMeta` for middleware errors).
- The duplicate WS path error of `WebsocketVatel.RegisterEndpoint` is a plain error
  "endpoint WS <path>: already registered" (v1.24.2: an `*errors.Error` with a `path`
  field).
- `ListenAndServe` after a failed `BuildHandlers` returns "previous build failed" (v1.24.2
  started the server with part of the routes).
- Go 1.26, fasthttp v1.77.0 and fasthttp/router v1.5.4 are required (v1.24.2: Go 1.24,
  fasthttp v1.31.0, router v1.4.4).
