package httpapi

import (
	"github.com/go-chi/chi/v5"

	"github.com/automoto/gg-scale/internal/entitlement"
)

// mountInternalAPI mounts the /internal surface — currently only the
// declarative entitlement API the external billing service drives. It is a
// sibling of /metrics, deliberately outside the huma /v1 group so it never
// enters openapi.yaml or the SDKs. An empty token leaves the whole subtree
// unmounted (404), the self-host default.
func mountInternalAPI(r chi.Router, d Deps) {
	if d.EntitlementAPIToken == "" {
		return
	}
	r.Mount("/internal/entitlements", entitlement.New(entitlement.Deps{
		Pool:     d.Pool,
		Mailer:   d.Mailer,
		MailFrom: d.MailFrom,
		Token:    d.EntitlementAPIToken,
		Metrics:  d.Metrics,
		// An apply changes the tier (held by the API key cache) and the
		// feature grants. Both drops are safe on nil.
		TenantChanged: func(tenantID int64) {
			d.APIKeyCache.Invalidate(tenantID)
			d.RBAC.InvalidateFeatureGrants(tenantID)
		},
	}))
}
