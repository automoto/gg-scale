package httpapi

import (
	"context"
	"log/slog"

	"github.com/go-chi/chi/v5"

	"github.com/ggscale/ggscale/internal/entitlement"
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
		Pool:       d.Pool,
		Mailer:     d.Mailer,
		MailFrom:   d.MailFrom,
		Token:      d.EntitlementAPIToken,
		Metrics:    d.Metrics,
		ReloadRBAC: reloadRBACPolicy(d),
	}))
}

// reloadRBACPolicy adapts the authorizer to the entitlement package's
// callback: grant changes must refresh the casbin policy without waiting for
// an unrelated reload. nil-safe for tests built without RBAC.
func reloadRBACPolicy(d Deps) func(ctx context.Context) {
	if d.RBAC == nil {
		return nil
	}
	return func(ctx context.Context) {
		if err := d.RBAC.ReloadPolicy(); err != nil {
			slog.WarnContext(ctx, "rbac reload after entitlement apply", "err", err)
		}
	}
}
