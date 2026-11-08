package vatel

// MiddlewarePos identifies the pipeline position where a middleware executes.
type MiddlewarePos int

const (
	OnRequest        MiddlewarePos = iota // first: IP rate limit, request logging
	OnAuthentication                      // decode token, populate request context
	OnAuthorization                       // check required permissions per endpoint
	OnHandle                              // pre-handler: per-user rate limit, audit, tx setup
	OnSuccessResponse
	OnErrorResponse
)

// EndpointMeta carries read-only metadata about the endpoint being served.
// It is passed to every middleware function so middleware can make decisions
// based on the endpoint's static configuration (route, permissions, log level)
// without accessing internal endpoint state.
type EndpointMeta struct {
	Method     string
	Path       string
	Perms      []string
	LogOptions LogOption
}

// middlewareSet holds middleware slices for each pipeline position.
// Size 6 matches the six MiddlewarePos constants (0–5).
type middlewareSet [6][]func(Context, EndpointMeta) error
