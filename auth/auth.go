// Package auth provides JWT-based authentication and authorization middleware
// for vatel HTTP endpoints.
//
// It is the recommended starting point for green-field projects. Wire it once
// globally (token decode) and per-endpoint (permission check):
//
//	cfg := &auth.Config{
//	    Decoder:    myJWTDecoder,
//	    Authorizer: myAuthorizer,
//	}
//
//	v := vatel.NewVatel()
//	v.AddMiddleware(vatel.OnAuthentication, cfg.Middleware())
//
//	v.Add(
//	    vatel.GET("/orders",      ordersCtrl, cfg.RequirePerms(map[string]uint{"orders:read": 0})),
//	    vatel.POST("/orders",     createCtrl, cfg.RequirePerms(map[string]uint{"orders:write": 1})),
//	    vatel.GET("/healthcheck", healthCtrl),  // public — no RequirePerms
//	)
package auth

import (
	"encoding/json"
	"sort"

	"github.com/axkit/vatel"
)

// Authorizer checks whether a token's permission bytes satisfy the required
// bit positions. This is the bitset-based permission model used by this package.
// Implement it via github.com/axkit/bitset or any compatible library.
type Authorizer interface {
	IsAllowed(requestPerms []byte, endpointPerms ...uint) (bool, error)
}

// Config holds the auth dependencies for HTTP endpoints.
type Config struct {
	// Decoder decodes the Authorization header value into a Tokener. It is
	// required when Middleware is used. It receives the raw header value:
	// nothing strips a "Bearer " prefix, so strip it in the decoder if your
	// clients send one. If Decoder is nil, a request carrying an
	// Authorization header is treated like one with a bad token: public
	// endpoints serve it as unauthenticated, RequireAuth and RequirePerms
	// reject it with vatel.ErrInternalError (500).
	Decoder vatel.TokenDecoder

	// Authorizer checks whether the token's permission bytes satisfy the
	// endpoint's required permissions. It is required by RequirePerms; a nil
	// Authorizer fails RequirePerms endpoints with vatel.ErrInternalError (500).
	Authorizer Authorizer

	// RevokeCheck optionally verifies that the token has not been revoked
	// (omit it for stateless JWTs). Like Decoder, it receives the raw
	// Authorization header value. If it returns an error, RequireAuth and
	// RequirePerms endpoints fail with vatel.ErrInternalError (500); public
	// endpoints serve the request as unauthenticated.
	RevokeCheck vatel.RevokeTokenChecker

	// Disabled skips all auth checks. Useful for tests and local development.
	// Set it before the server starts serving; it is read on every request
	// without synchronisation, so changing it while requests are served is a
	// data race.
	Disabled bool
}

// tokenErrKey is the context key under which Middleware stores a token
// error (invalid, expired or revoked token, failed revocation check or unset
// Decoder) for RequireAuth/RequirePerms.
const tokenErrKey = "authTokenError"

// Middleware returns a OnAuthentication middleware that:
//  1. Reads the Authorization header.
//  2. Optionally checks token revocation (if RevokeCheck is set).
//  3. Decodes the token and stores the payload on ctx via SetTokenPayload.
//  4. If the payload's Debug() is true, calls ctx.SetVerboseError(true) to
//     enable detailed error responses for this request.
//
// A bad token (invalid, expired, revoked) does not fail the request here:
// public endpoints ignore it and serve the request as unauthenticated.
// RequireAuth and RequirePerms reject the request with the token error
// (401). A failed revocation check or an unset Decoder is handled the same
// way, except that RequireAuth and RequirePerms reject with 500.
//
// Endpoints that require authentication or specific permissions should add
// RequirePerms (or RequireAuth) as an additional EndpointOption.
func (c *Config) Middleware() func(vatel.Context, vatel.EndpointMeta) error {
	return func(ctx vatel.Context, _ vatel.EndpointMeta) error {
		if c.Disabled {
			return nil
		}

		at := ctx.Header("Authorization")
		if len(at) == 0 {
			return nil // no token present; RequireAuth/RequirePerms will reject if needed
		}

		ap, err := c.decode(at)
		if err != nil {
			ctx.Set(tokenErrKey, tokenErr{err})
			return nil
		}
		ctx.SetTokenPayload(ap)

		// Enable verbose errors if the token requests debug mode.
		if ap != nil && ap.Debug() {
			ctx.SetVerboseError(true)
		}

		return nil
	}
}

func (c *Config) decode(at []byte) (vatel.AuthPayload, error) {
	if c.Decoder == nil {
		return nil, vatel.ErrInternalError.New().Msg("auth: Config.Decoder is not set")
	}
	if c.RevokeCheck != nil {
		revoked, err := c.RevokeCheck.IsTokenRevoked(string(at))
		if err != nil {
			return nil, vatel.ErrInternalError.Wrap(err).Msg("auth: token revocation check failed")
		}
		if revoked {
			return nil, vatel.ErrAccessTokenRevoked.New()
		}
	}

	token, err := c.Decoder.Decode(at)
	if err != nil {
		return nil, vatel.ErrUnauthorized.Wrap(err)
	}
	return token.ApplicationPayload(), nil
}

// tokenErr wraps a token error stored in the context. vatel logs context
// values as JSON, so it marshals to the error text instead of "{}".
type tokenErr struct{ err error }

func (t tokenErr) MarshalJSON() ([]byte, error) { return json.Marshal(t.err.Error()) }

// tokenError returns the error Middleware stored for a bad token, or nil.
func tokenError(ctx vatel.Context) error {
	if t, ok := ctx.Get(tokenErrKey).(tokenErr); ok {
		return t.err
	}
	return nil
}

// RequireAuth returns an EndpointOption that adds an OnAuthorization
// middleware rejecting requests that carry no token payload. Use this for
// endpoints that require any valid authenticated user but no specific
// permission.
func (c *Config) RequireAuth() vatel.EndpointOption {
	return func(e *vatel.Endpoint) {
		e.AddMiddleware(vatel.OnAuthorization, func(ctx vatel.Context, _ vatel.EndpointMeta) error {
			if c.Disabled {
				return nil
			}
			if ctx.TokenPayload() == nil {
				if err := tokenError(ctx); err != nil {
					return err
				}
				return vatel.ErrUnauthorized.New().Msg("authentication required")
			}
			return nil
		})
	}
}

// RequirePerms returns an EndpointOption that adds an OnAuthorization
// middleware verifying the token holds all of the named permissions.
//
// perms maps each permission name to its bit position in the token's
// permission bytes. Bit positions are passed directly to Authorizer.IsAllowed;
// names are used for error reporting and are appended, sorted, to the
// endpoint's Perms, so middleware sees them in EndpointMeta.Perms.
//
// Config.Disabled is checked on every request, so it may be set after the
// option is created, but before the server starts serving (see
// Config.Disabled). A nil Config.Authorizer fails the request with
// vatel.ErrInternalError (500).
func (c *Config) RequirePerms(perms map[string]uint) vatel.EndpointOption {
	names := make([]string, 0, len(perms))
	for name := range perms {
		names = append(names, name)
	}
	sort.Strings(names)

	bits := make([]uint, 0, len(perms))
	for _, name := range names {
		bits = append(bits, perms[name])
	}

	return func(e *vatel.Endpoint) {
		e.Perms = append(e.Perms, names...)
		e.AddMiddleware(vatel.OnAuthorization, func(ctx vatel.Context, _ vatel.EndpointMeta) error {
			if c.Disabled {
				return nil
			}

			tp := ctx.TokenPayload()
			if tp == nil {
				if err := tokenError(ctx); err != nil {
					return err
				}
				return vatel.ErrUnauthorized.New().
					Set("perms", names).
					Msg("authentication required")
			}

			// TokenPayload is any: a custom middleware may have set a payload
			// that is not a vatel.AuthPayload. Only Perms is needed here.
			pa, ok := tp.(interface{ Perms() []byte })
			if !ok {
				return vatel.ErrUnauthorized.New().
					Set("perms", names).
					Msg("token payload does not carry permissions")
			}

			if c.Authorizer == nil {
				return vatel.ErrInternalError.New().Msg("auth: Config.Authorizer is not set")
			}

			allowed, err := c.Authorizer.IsAllowed(pa.Perms(), bits...)
			if err != nil {
				return vatel.ErrUnauthorized.Wrap(err).Set("perms", names)
			}
			if !allowed {
				return vatel.ErrForbidden.New().Set("perms", names)
			}
			return nil
		})
	}
}
