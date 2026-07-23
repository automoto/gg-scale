package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/matchmaker"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/quota"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/relay"
)

// Per-player relay credential issuance is rate limited so a single player
// cannot burn the tenant's monthly session allowance in a burst. TTL-driven
// re-issuance needs roughly one credential per credential-TTL, so a steady
// ~12/min with a burst of 10 leaves ample headroom for reconnect flaps while
// still bounding abuse to a slow trickle.
const (
	relayIssueRatePerSecond = 0.2
	relayIssueBurst         = 10
)

// relayCredentialsInput optionally scopes the request to a match. When MatchID
// is set the server verifies the caller is in that match's roster before
// issuing, so a peer-to-peer client can prove match membership when asking for
// relay credentials. Unqualified requests stay valid for non-matchmade P2P
// (server browser, direct invites).
type relayCredentialsInput struct {
	MatchID string `query:"match_id"`
}

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

func relayCredentials(d Deps) func(context.Context, *relayCredentialsInput) (*relayCredentialsOutput, error) {
	return func(ctx context.Context, in *relayCredentialsInput) (*relayCredentialsOutput, error) {
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
		// Per-player issuance rate limit: bounds burst abuse of the monthly
		// allowance. Checked first among the abuse gates (before the ban DB
		// query, match scoping, and metering) so a throttled caller costs only
		// the in-memory bucket lookup.
		if d.Limiter != nil {
			key := fmt.Sprintf("relay_issue:%d:%d", tenantID, playerID)
			dec, lerr := d.Limiter.Allow(ctx, key, relayIssueRatePerSecond, relayIssueBurst)
			if lerr != nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			if !dec.Allowed {
				d.Metrics.RelayIssueThrottled()
				return nil, huma.Error429TooManyRequests("relay credential requests are being throttled; retry shortly")
			}
		}
		// Tenant-ban enforcement point: a banned account can't get relay
		// credentials.
		if banned, berr := playerTenantBanned(ctx, d, playerID); berr != nil {
			return nil, huma.Error500InternalServerError("internal error")
		} else if banned {
			return nil, huma.Error403Forbidden("account banned")
		}
		// Optional match scoping: prove roster membership before issuing. Done
		// before metering so a rejected request never consumes the allowance.
		if in.MatchID != "" {
			if merr := verifyRelayMatchMembership(ctx, d, projectID, playerID, in.MatchID); merr != nil {
				return nil, merr
			}
		}
		// Monthly session allowance: refuses only new issuance — in-flight
		// TURN sessions are unaffected.
		if d.RelayMeter != nil {
			if err := d.RelayMeter.Allow(ctx, tenantID); err != nil {
				var qe *quota.ErrQuotaExceeded
				if errors.As(err, &qe) {
					d.Metrics.QuotaRejection(qe.Axis)
					return nil, huma.Error403Forbidden("relay session allowance for this month is used up")
				}
				return nil, huma.Error500InternalServerError("internal error")
			}
		}
		creds, err := d.RelayIssuer.Issue(tenantID, playerID)
		if err != nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		d.Metrics.RelayCredentialIssued()
		return &relayCredentialsOutput{Body: *creds}, nil
	}
}

// verifyRelayMatchMembership confirms the caller belongs to the given match's
// roster in this project. A missing match, a cross-project match, or a
// non-member all return the same 403 so match ids can't be enumerated. Returns
// nil when the player is a member.
func verifyRelayMatchMembership(ctx context.Context, d Deps, projectID, playerID int64, matchID string) error {
	if d.Matchmaker == nil {
		return huma.Error400BadRequest("match scoping unavailable")
	}
	match, err := d.Matchmaker.GetMatch(ctx, matchID)
	if errors.Is(err, matchmaker.ErrNotFound) {
		return huma.Error403Forbidden("forbidden")
	}
	if err != nil {
		return huma.Error500InternalServerError("internal error")
	}
	if match.ProjectID != projectID {
		return huma.Error403Forbidden("forbidden")
	}
	for _, r := range match.Roster {
		if r.PlayerID == playerID {
			return nil
		}
	}
	return huma.Error403Forbidden("forbidden")
}
