package main

import (
	"github.com/axkit/errors"
	"github.com/axkit/vatel"
	"github.com/axkit/vatel/auth"
)

// User is the domain model returned by user endpoints.
type User struct {
	ID    int    `json:"id"`
	Login string `json:"login"`
	Email string `json:"email"`
	Role  int    `json:"role"`
}

var errUserNotFound = errors.Template("user not found").StatusCode(404)

// in-memory store for example purposes — replace with a real repository.
var userStore = []User{
	{ID: 100, Login: "alice", Email: "alice@example.com", Role: 1},
	{ID: 101, Login: "bob", Email: "bob@example.com", Role: 2},
}

// ── Get User ──────────────────────────────────────────────────────────────────
// GET /users/{id}

type GetUserController struct {
	vatel.PathParams[struct {
		ID int `param:"id"`
	}]
	vatel.Output[User]
}

func (c *GetUserController) Handle(_ vatel.Context) error {
	for _, u := range userStore {
		if u.ID == c.Params.ID {
			c.Out = u
			return nil
		}
	}
	return errUserNotFound.New().Set("id", c.Params.ID)
}

// ── Get Current User ──────────────────────────────────────────────────────────
// GET /users/me  — returns the profile of the authenticated caller.

type GetMeController struct {
	vatel.Output[User]
}

func (c *GetMeController) Handle(ctx vatel.Context) error {
	// RequireAuth guarantees a payload unless auth.Config.Disabled is set.
	claims, ok := ctx.TokenPayload().(*Claims)
	if !ok {
		return vatel.ErrUnauthorized.New()
	}
	for _, u := range userStore {
		if u.ID == claims.UserID {
			c.Out = u
			return nil
		}
	}
	return errUserNotFound.New().Set("id", claims.UserID)
}

// ── Endpoint Registration ─────────────────────────────────────────────────────

func UserEndpoints(cfg *auth.Config) vatel.Endpoints {
	read := cfg.RequirePerms(map[string]uint{"users:read": Permissions["users:read"]})

	return vatel.Endpoints{
		vatel.GET("/users/me", func() vatel.Handler { return &GetMeController{} }, cfg.RequireAuth()),
		vatel.GET("/users/{id}", func() vatel.Handler { return &GetUserController{} }, read),
	}
}
