package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/relay"
)

// relayCredentialsOutput carries the TURN-REST credentials. The password field
// is the TURN-REST HMAC, intentionally returned to the authenticated player so
// they can authenticate against the relay — not a secret-at-rest leak.
type relayCredentialsOutput struct {
	Body relay.Credentials
}

func registerRelay(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "issueRelayCredentials",
		Method:      http.MethodPost,
		Path:        "/v1/relay/credentials",
		Summary:     "Issue short-lived TURN-REST relay credentials",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, relayCredentials(d))
}

func relayCredentials(d Deps) func(context.Context, *struct{}) (*relayCredentialsOutput, error) {
	return func(ctx context.Context, _ *struct{}) (*relayCredentialsOutput, error) {
		tenantID, err := db.TenantFromContext(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		playerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		projectID, ok := playerauth.ProjectIDFromContext(ctx)
		if !ok {
			projectID, ok = db.ProjectFromContext(ctx)
		}
		if !ok {
			return nil, huma.Error403Forbidden("no project")
		}
		if d.RBAC == nil {
			return nil, huma.Error500InternalServerError("authorization unavailable")
		}
		allowed, err := d.RBAC.CanPlayer(tenantID, playerID, rbac.ProjectRelayObject(projectID), rbac.ActionIssueCredentials)
		if err != nil {
			return nil, huma.Error500InternalServerError("authorization check failed")
		}
		if !allowed {
			return nil, huma.Error403Forbidden("forbidden")
		}
		enabled, err := d.RBAC.FeatureEnabled(ctx, tenantID, projectID, rbac.FeatureP2PRelay)
		if err != nil {
			return nil, huma.Error500InternalServerError("feature check failed")
		}
		if !enabled {
			return nil, huma.Error403Forbidden("forbidden")
		}
		// Tenant-ban enforcement point: a banned account can't get relay
		// credentials.
		if banned, berr := playerTenantBanned(ctx, d, playerID); berr != nil {
			return nil, huma.Error500InternalServerError("internal error")
		} else if banned {
			return nil, huma.Error403Forbidden("account banned")
		}
		creds, err := d.RelayIssuer.Issue(tenantID, playerID)
		if err != nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		d.Metrics.RelayCredentialIssued()
		return &relayCredentialsOutput{Body: *creds}, nil
	}
}
