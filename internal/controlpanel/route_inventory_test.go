package controlpanel_test

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/controlpanel"
	"github.com/automoto/gg-scale/internal/verifycode"
)

// guardTier classifies the authorization a mutating route must sit behind.
type guardTier string

const (
	// tierPublic: pre-session, gated by a secret/limiter (login, setup, verify,
	// magic-link acceptance).
	tierPublic guardTier = "public"
	// tierSession: any authenticated user, acting only on their own account.
	tierSession guardTier = "session"
	// tierTenantAdmin: requireTenantAccess(roleAdmin); finer owner-only or
	// platform-admin-only actions add an in-handler check on top.
	tierTenantAdmin guardTier = "tenant-admin"
	// tierPlatformAdmin: requirePlatformAdmin.
	tierPlatformAdmin guardTier = "platform-admin"
)

// postRouteGuards pins every mutating (POST) control-panel route to its
// intended authorization tier. chi.Walk cannot see middleware, so this is a
// tripwire, not a proof: adding, removing, or renaming a POST route fails this
// test until it is classified here — the forcing function to decide a new
// route's authorization and add a negative-authz test for it. A route that
// merely *moves* to a weaker middleware group keeps its pattern and is caught
// instead by the behavioural tests (e.g. TestControlPanelCreateTenant_non_platform_admin_denied).
var postRouteGuards = map[string]guardTier{
	"/setup/token":           tierPublic,
	"/setup":                 tierPublic,
	"/login":                 tierPublic,
	"/login/2fa":             tierPublic,
	"/forgot-password":       tierPublic,
	"/reset-password":        tierPublic,
	"/verify":                tierPublic,
	"/verify/resend":         tierPublic,
	"/request-access":        tierPublic,
	"/request-access/accept": tierPublic,
	"/invite/accept":         tierPublic,

	"/account/password":         tierSession,
	"/account/2fa/setup":        tierSession,
	"/account/2fa/confirm":      tierSession,
	"/account/2fa/disable":      tierSession,
	"/account/2fa/backup-codes": tierSession,
	"/logout":                   tierSession,

	"/tenants":                                   tierPlatformAdmin,
	"/admin/team/invite":                         tierPlatformAdmin,
	"/admin/tenant-signups/config":               tierPlatformAdmin,
	"/admin/tenant-signups/{id}/approve":         tierPlatformAdmin,
	"/admin/tenant-signups/{id}/deny":            tierPlatformAdmin,
	"/admin/change-requests/{id}/approve":        tierPlatformAdmin,
	"/admin/change-requests/{id}/deny":           tierPlatformAdmin,
	"/admin/users/{userID}/disable":              tierPlatformAdmin,
	"/admin/users/{userID}/enable":               tierPlatformAdmin,
	"/admin/player-accounts/{accountID}/disable": tierPlatformAdmin,
	"/admin/player-accounts/{accountID}/enable":  tierPlatformAdmin,

	"/tenants/{tenantID}/api-keys":                                                 tierTenantAdmin,
	"/tenants/{tenantID}/api-keys/{apiKeyID}/label":                                tierTenantAdmin,
	"/tenants/{tenantID}/api-keys/{apiKeyID}/features":                             tierTenantAdmin,
	"/tenants/{tenantID}/api-keys/{apiKeyID}/revoke":                               tierTenantAdmin,
	"/tenants/{tenantID}/projects":                                                 tierTenantAdmin,
	"/tenants/{tenantID}/projects/{projectID}/allocations":                         tierTenantAdmin,
	"/tenants/{tenantID}/projects/{projectID}/allocations/{allocID}/deallocate":    tierTenantAdmin,
	"/tenants/{tenantID}/projects/{projectID}/fleets":                              tierTenantAdmin,
	"/tenants/{tenantID}/projects/{projectID}/fleets/{fleetID}":                    tierTenantAdmin,
	"/tenants/{tenantID}/projects/{projectID}/fleets/{fleetID}/delete":             tierTenantAdmin,
	"/tenants/{tenantID}/projects/{projectID}/leaderboards":                        tierTenantAdmin,
	"/tenants/{tenantID}/projects/{projectID}/leaderboards/{leaderboardID}":        tierTenantAdmin,
	"/tenants/{tenantID}/projects/{projectID}/leaderboards/{leaderboardID}/delete": tierTenantAdmin,
	"/tenants/{tenantID}/projects/{projectID}/config":                              tierTenantAdmin,
	"/tenants/{tenantID}/projects/{projectID}/steam-auth":                          tierTenantAdmin,
	"/tenants/{tenantID}/projects/{projectID}/players/invite":                      tierTenantAdmin,
	"/tenants/{tenantID}/projects/{projectID}/players/{playerID}/ban":              tierTenantAdmin,
	"/tenants/{tenantID}/projects/{projectID}/players/{playerID}/disable":          tierTenantAdmin,
	"/tenants/{tenantID}/projects/{projectID}/players/{playerID}/link":             tierTenantAdmin,
	"/tenants/{tenantID}/rate-limits/api":                                          tierTenantAdmin,
	"/tenants/{tenantID}/rate-limits/invites/recipient":                            tierTenantAdmin,
	"/tenants/{tenantID}/rate-limits/projects/{projectID}/invites":                 tierTenantAdmin,
	"/tenants/{tenantID}/rate-limits/storage":                                      tierTenantAdmin,
	"/tenants/{tenantID}/rate-limits/projects/{projectID}/storage":                 tierTenantAdmin,
	"/tenants/{tenantID}/rate-limits/quotas":                                       tierTenantAdmin,
	"/tenants/{tenantID}/settings/tier":                                            tierTenantAdmin,
	"/tenants/{tenantID}/settings/custom-token":                                    tierTenantAdmin,
	"/tenants/{tenantID}/settings/features":                                        tierTenantAdmin,
	"/tenants/{tenantID}/settings/change-requests":                                 tierTenantAdmin,
	"/tenants/{tenantID}/settings/disable":                                         tierTenantAdmin,
	"/tenants/{tenantID}/settings/enable":                                          tierTenantAdmin,
	"/tenants/{tenantID}/team/invite":                                              tierTenantAdmin,
	"/tenants/{tenantID}/team/invites/{inviteID}/revoke":                           tierTenantAdmin,
	"/tenants/{tenantID}/team/members/{userID}/roles":                              tierTenantAdmin,
	"/tenants/{tenantID}/team/members/{membershipID}/remove":                       tierTenantAdmin,
}

func TestControlPanelPostRoutesAreClassified(t *testing.T) {
	h := controlpanel.New(controlpanel.Deps{VerifySigningKey: make([]byte, verifycode.SigningKeySize)})
	routes, ok := h.(chi.Routes)
	require.True(t, ok)

	seen := map[string]bool{}
	require.NoError(t, chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method != http.MethodPost {
			return nil
		}
		seen[route] = true
		assert.Containsf(t, postRouteGuards, route,
			"new POST route %q is unclassified — add it to postRouteGuards with its guard tier and a negative-authz test", route)
		return nil
	}))
	for route := range postRouteGuards {
		assert.Truef(t, seen[route], "classified route %q no longer registered — remove it from postRouteGuards", route)
	}
}
