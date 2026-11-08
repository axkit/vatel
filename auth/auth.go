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

	"github.com/axkit/vatel"
)

// Authorizer checks whether a token's permission bytes satisfy the required
// bit positions. This is the bitset-based permission model used by this package.
// Implement it via github.com/axkit/bitset or any compatible library.
type Authorizer interface {
	IsAllowed(requestPerms []byte, endpointPerms ...uint) (bool, error)
}

// Config holds the auth dependencies for HTTP endpoints. All fields are
// optional — omit those you don't need (e.g. RevokeCheck for stateless JWTs).
type Config struct {
	// Decoder decodes the raw Authorization header value into a Tokener.
	Decoder vatel.TokenDecoder

	// Authorizer checks whether the token's permission bytes satisfy the
	// endpoint's required permissions.
	Authorizer Authorizer

	// RevokeCheck optionally verifies that the token has not been revoked.
	RevokeCheck vatel.RevokeTokenChecker

	// Disabled skips all auth checks. Useful for tests and local development.
	Disabled bool
}

// tokenErrKey is the context key under which Middleware stores a token
// error (invalid, expired or revoked token) for RequireAuth/RequirePerms.
const tokenErrKey = "authTokenError"

// Middleware returns a OnAuthentication middleware that:
//  1. Reads the Authorization header.
//  2. Optionally checks token revocation (if RevokeCheck is set).
//  3. Decodes the token and stores the payload on ctx via SetTokenPayload.
//  4. If the payload implements interface{ Debug() bool } and Debug() is true,
//     calls ctx.SetVerboseError(true) to enable detailed error responses for
//     this request.
//
// A bad token (invalid, expired, revoked) does not fail the request here:
// public endpoints ignore it and serve the request as unauthenticated.
// RequireAuth and RequirePerms reject the request with the token error.
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
		if d, ok := ap.(interface{ Debug() bool }); ok && d.Debug() {
			ctx.SetVerboseError(true)
		}

		return nil
	}
}

func (c *Config) decode(at []byte) (vatel.AuthPayload, error) {
	if c.RevokeCheck != nil {
		revoked, err := c.RevokeCheck.IsTokenRevoked(string(at))
		if err != nil {
			return nil, err
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
// permission bytes. Names are used only for error reporting; bit positions
// are passed directly to Authorizer.IsAllowed.
func (c *Config) RequirePerms(perms map[string]uint) vatel.EndpointOption {
	if c.Disabled {
		return func(*vatel.Endpoint) {}
	}

	bits := make([]uint, 0, len(perms))
	names := make([]string, 0, len(perms))
	for name, bit := range perms {
		names = append(names, name)
		bits = append(bits, bit)
	}

	return func(e *vatel.Endpoint) {
		e.AddMiddleware(vatel.OnAuthorization, func(ctx vatel.Context, _ vatel.EndpointMeta) error {
			tp := ctx.TokenPayload()
			if tp == nil {
				if err := tokenError(ctx); err != nil {
					return err
				}
				return vatel.ErrUnauthorized.New().
					Set("perms", names).
					Msg("authentication required")
			}

			pa, ok := tp.(interface{ Perms() []byte })
			if !ok {
				return vatel.ErrUnauthorized.New().
					Set("perms", names).
					Msg("token payload does not carry permissions")
			}

			if c.Authorizer == nil {
				panic("vatel/auth: Config.Authorizer must be set to use RequirePerms")
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
