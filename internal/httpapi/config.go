package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

type remoteConfigInput struct {
	IfNoneMatch string `header:"If-None-Match" doc:"ETag from a previous config response." example:"\"remote-config-5f36b2ea290645ee34d943220a14b54ee5ea5be5b2ac97498f75ee173db9e9df\""`
}

type remoteConfigOutput struct {
	Status       int
	ETag         string `header:"ETag" doc:"Validator for the current project config."`
	CacheControl string `header:"Cache-Control"`
	Body         json.RawMessage
}

func registerRemoteConfig(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "getRemoteConfig",
		Method:      http.MethodGet,
		Path:        "/v1/config",
		Summary:     "Get the project's remote config",
		Description: "Returns the project-defined JSON object using only a project-pinned tenant API key. " +
			"Send If-None-Match with a previous ETag to receive 304 when unchanged.",
		Tags:     []string{"Remote Config"},
		Security: apiKeySecurity,
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Current remote config",
				Content: map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{
					Type:                 huma.TypeObject,
					Description:          "Arbitrary project-defined JSON key-value map.",
					AdditionalProperties: true,
					Examples: []any{map[string]any{
						"minimum_client_version":  "1.4.0",
						"maintenance_mode":        false,
						"daily_reward_multiplier": 1.5,
					}},
				}}},
			},
			"304": {Description: "Config has not changed; response has no body"},
		},
	}, remoteConfigGet(d))
}

func remoteConfigGet(d Deps) func(context.Context, *remoteConfigInput) (*remoteConfigOutput, error) {
	return func(ctx context.Context, in *remoteConfigInput) (*remoteConfigOutput, error) {
		projectID, _, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}

		var config []byte
		err = d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
			var err error
			config, err = sqlcgen.New(tx).GetRemoteConfig(ctx, projectID)
			return err
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("project not found")
		}
		if err != nil {
			return nil, serverError(ctx, "remote config: get", err)
		}

		etag := remoteConfigETag(config)
		out := &remoteConfigOutput{
			Status:       http.StatusOK,
			ETag:         etag,
			CacheControl: "no-cache",
			Body:         json.RawMessage(config),
		}
		if etagMatches(in.IfNoneMatch, etag) {
			out.Status = http.StatusNotModified
			out.Body = nil
		}
		return out, nil
	}
}

func remoteConfigETag(config []byte) string {
	sum := sha256.Sum256(config)
	return `"remote-config-` + hex.EncodeToString(sum[:]) + `"`
}

// etagMatches applies the weak comparison required for If-None-Match on GET.
// Remote-config validators never contain commas, so a comma-separated scan is
// sufficient for validators emitted by this endpoint.
func etagMatches(header, current string) bool {
	for validator := range strings.SplitSeq(header, ",") {
		validator = strings.TrimSpace(validator)
		if validator == "*" {
			return true
		}
		validator = strings.TrimPrefix(validator, "W/")
		if validator == current {
			return true
		}
	}
	return false
}
