package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

type healthzResult struct {
	Status  string `json:"status" example:"ok"`
	Version string `json:"version" example:"1.0.0"`
	Commit  string `json:"commit" example:"abc1234"`
}

type healthzOutput struct {
	Body healthzResult
}

// registerHealthz registers the public liveness probe. No auth.
func registerHealthz(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "healthz",
		Method:      http.MethodGet,
		Path:        "/v1/healthz",
		Summary:     "Liveness probe",
		Tags:        []string{"Health"},
	}, func(_ context.Context, _ *struct{}) (*healthzOutput, error) {
		return &healthzOutput{Body: healthzResult{
			Status: "ok", Version: d.Version, Commit: d.Commit,
		}}, nil
	})
}
