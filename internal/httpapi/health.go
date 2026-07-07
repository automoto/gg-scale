package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

type healthzResult struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
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
		Tags:        []string{"/v1"},
	}, func(_ context.Context, _ *struct{}) (*healthzOutput, error) {
		return &healthzOutput{Body: healthzResult{
			Status: "ok", Version: d.Version, Commit: d.Commit,
		}}, nil
	})
}
