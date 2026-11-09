package main

import "github.com/axkit/vatel"

// HealthStatus is the response body for the healthcheck endpoint.
type HealthStatus struct {
	Status string `json:"status"`
}

// HealthcheckController is a public endpoint — no RequirePerms / RequireAuth.
type HealthcheckController struct {
	vatel.Output[HealthStatus]
}

func (c *HealthcheckController) Handle(_ vatel.Context) error {
	c.Out = HealthStatus{Status: "ok"}
	return nil
}

func HealthEndpoints() vatel.Endpoints {
	return vatel.Endpoints{
		vatel.GET("/healthcheck", func() vatel.Handler { return &HealthcheckController{} }),
	}
}
