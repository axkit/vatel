package vatel

import "time"

// EndpointOption is a functional option for configuring an Endpoint.
type EndpointOption func(*Endpoint)

// Perms appends permission names to the endpoint's metadata (EndpointMeta.Perms).
//
// On HTTP endpoints Perms is metadata only: vatel does not check it. The check
// is done by middleware, e.g. auth.RequirePerms, which also appends its
// permission names here. On WS endpoints non-empty Perms require
// WithWsAuthFunc, which receives them; without it BuildHandlers fails.
func Perms(p ...string) EndpointOption {
	return func(e *Endpoint) {
		e.Perms = append(e.Perms, p...)
	}
}

// WithCompress enables gzip compression for the endpoint response.
func WithCompress() EndpointOption {
	return func(e *Endpoint) { e.Compress = true }
}

// WithLogOption sets the log verbosity bitmask for the endpoint.
func WithLogOption(lo LogOption) EndpointOption {
	return func(e *Endpoint) { e.LogOptions = lo }
}

// WithSuccessStatusCode sets a custom HTTP status code for successful responses.
// Default is 200.
func WithSuccessStatusCode(code int) EndpointOption {
	return func(e *Endpoint) { e.SuccessStatusCode = code }
}

// WithContentType sets a custom response Content-Type header.
// Default is "application/json; charset=utf-8".
func WithContentType(ct string) EndpointOption {
	return func(e *Endpoint) { e.ResponseContentType = ct }
}

// WithManualStatusCode disables automatic status code setting, allowing
// the handler to control it explicitly via ctx.SetStatusCode().
func WithManualStatusCode() EndpointOption {
	return func(e *Endpoint) { e.ManualStatusCode = true }
}

// WithOptionalBody allows an empty request body on a POST/PUT/PATCH endpoint.
// When the body is empty or contains only whitespace, decoding is skipped: the
// input stays zero and Validator.Validate() is not called. A non-empty body is
// decoded and validated as usual; invalid JSON is still rejected with 400.
// Using it on a method without a request body (GET, DELETE, HEAD, OPTIONS) is
// a compile error returned from BuildHandlers.
func WithOptionalBody() EndpointOption {
	return func(e *Endpoint) { e.OptionalBody = true }
}

// WithTimeout sets the timeout of the request context returned by
// Context.Context(); it overrides WithDefaultTimeout. The timeout does not
// interrupt the handler by itself: only code that listens to the context
// (database queries, HTTP calls, select on Done()) stops. A handler returning
// context.DeadlineExceeded gets 504, context.Canceled gets 503.
func WithTimeout(d time.Duration) EndpointOption {
	return func(e *Endpoint) { e.Timeout = d }
}

// WithLanguageLabel sets a language label for the endpoint (for i18n use).
func WithLanguageLabel(label string) EndpointOption {
	return func(e *Endpoint) { e.LanguageLabel = label }
}

// GET creates a GET endpoint with the given path, controller factory, and options.
func GET(path string, ctrl func() Handler, opts ...EndpointOption) Endpoint {
	return buildEndpoint("GET", path, ctrl, opts...)
}

// POST creates a POST endpoint with the given path, controller factory, and options.
func POST(path string, ctrl func() Handler, opts ...EndpointOption) Endpoint {
	return buildEndpoint("POST", path, ctrl, opts...)
}

// PUT creates a PUT endpoint with the given path, controller factory, and options.
func PUT(path string, ctrl func() Handler, opts ...EndpointOption) Endpoint {
	return buildEndpoint("PUT", path, ctrl, opts...)
}

// PATCH creates a PATCH endpoint with the given path, controller factory, and options.
func PATCH(path string, ctrl func() Handler, opts ...EndpointOption) Endpoint {
	return buildEndpoint("PATCH", path, ctrl, opts...)
}

// DELETE creates a DELETE endpoint with the given path, controller factory, and options.
func DELETE(path string, ctrl func() Handler, opts ...EndpointOption) Endpoint {
	return buildEndpoint("DELETE", path, ctrl, opts...)
}

// HEAD creates a HEAD endpoint with the given path, controller factory, and options.
// Input is decoded from the URL query string, like GET. The fasthttp server
// drops the response body of a HEAD request, so only the status code and
// headers reach the client.
func HEAD(path string, ctrl func() Handler, opts ...EndpointOption) Endpoint {
	return buildEndpoint("HEAD", path, ctrl, opts...)
}

// OPTIONS creates an OPTIONS endpoint with the given path, controller factory, and options.
// Input is decoded from the URL query string, like GET. An explicit OPTIONS
// endpoint takes precedence over router.GlobalOPTIONS for its path. Do not put
// RequireAuth/RequirePerms on an endpoint that serves a CORS preflight: browsers
// send the preflight without the Authorization header.
func OPTIONS(path string, ctrl func() Handler, opts ...EndpointOption) Endpoint {
	return buildEndpoint("OPTIONS", path, ctrl, opts...)
}

// WS creates a WebSocket endpoint with the given path, controller factory, and options.
func WS(path string, ctrl func() WebsocketHandler, opts ...EndpointOption) Endpoint {
	e := Endpoint{EndpointMeta: EndpointMeta{Method: "WS", Path: path}, WebsocketController: ctrl}
	for _, o := range opts {
		o(&e)
	}
	return e
}

func buildEndpoint(method, path string, ctrl func() Handler, opts ...EndpointOption) Endpoint {
	e := Endpoint{EndpointMeta: EndpointMeta{Method: method, Path: path}, Controller: ctrl}
	for _, o := range opts {
		o(&e)
	}
	return e
}

// Endpoints is a slice of Endpoint that implements the Endpointer interface.
// Use it to group related endpoints in one place without defining a named type.
//
//	func (a *OrdersAPI) Endpoints() []vatel.Endpoint {
//	    return vatel.Endpoints{
//	        vatel.GET("/orders",      func() Handler { return &ListOrders{api: a} }, vatel.Perms("orders:read")),
//	        vatel.POST("/orders",     func() Handler { return &CreateOrder{api: a} }, vatel.Perms("orders:write")),
//	        vatel.GET("/orders/{id}", func() Handler { return &GetOrder{api: a} },   vatel.Perms("orders:read")),
//	    }
//	}
type Endpoints []Endpoint

// Endpoints implements the Endpointer interface.
func (e Endpoints) Endpoints() []Endpoint { return e }
