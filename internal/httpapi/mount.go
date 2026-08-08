package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/automoto/gg-scale/internal/observability"
	"github.com/automoto/gg-scale/internal/realtime"
	"github.com/automoto/gg-scale/internal/tenant"
)

func requireAPIKeyPermission(d Deps, obj, act string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if d.RBAC == nil {
				http.Error(w, "authorization unavailable", http.StatusInternalServerError)
				return
			}
			key, ok := tenant.APIKeyFromContext(r.Context())
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			allowed, err := d.RBAC.CanAPIKey(key, obj, act)
			if err != nil {
				http.Error(w, "authorization check failed", http.StatusInternalServerError)
				return
			}
			if !allowed {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func mountRealtimeRoutes(r chi.Router, d Deps) {
	if d.Hub == nil {
		return
	}
	heartbeat := d.RealtimeHeartbeat
	if heartbeat <= 0 {
		heartbeat = realtime.DefaultHeartbeatInterval
	}
	var lifecycle *wsLifecycle
	if d.Pool != nil {
		lifecycle = newWSLifecycle(d.Pool, heartbeat, d.Metrics)
	}
	base := realtime.Options{
		Hub:               d.Hub,
		Cache:             d.Cache,
		TenantCap:         d.TenantConnectionCap,
		EnvMaxPerTenant:   d.RealtimeMaxPerTenant,
		MaxPerPlayer:      d.RealtimeMaxPerPlayer,
		HeartbeatInterval: heartbeat,
	}
	r.Get("/ws", func(w http.ResponseWriter, req *http.Request) {
		opts := base
		if lifecycle != nil {
			if st, ok := lifecycle.register(req.Context()); ok {
				defer lifecycle.unregister(st)
				grace := lifecycle.grace()
				// Pure in-memory per heartbeat: the sweeper does the batched
				// DB work; the connection just reads its verdict. A non-nil
				// error fires once — the connection closes on it — so the
				// log/metric below record each lifecycle close exactly once,
				// with its reason.
				opts.Revalidate = func(context.Context) error {
					err := st.check(grace)
					if err == nil {
						return nil
					}
					reason := observability.RealtimeCloseRevoked
					if errors.Is(err, errRealtimeUnverifiable) {
						reason = observability.RealtimeCloseUnverifiable
					}
					d.Metrics.RealtimeLifecycleClose(reason)
					slog.InfoContext(req.Context(), "realtime: closing socket on lifecycle revalidation",
						"reason", reason, "err", err, "tenant_id", st.tenantID, "player_id", st.playerID)
					return err
				}
			}
		}
		realtime.ServeWS(opts)(w, req)
	})
}

// passwordResetEnqueuer binds the shared durable-job insert hook to one auth
// surface. Returns nil when no job queue is wired, which tells the web
// handler to fall back to in-process delivery.
func passwordResetEnqueuer(d Deps, surface string) func(context.Context, string) error {
	if d.EnqueuePasswordReset == nil {
		return nil
	}
	return func(ctx context.Context, email string) error {
		return d.EnqueuePasswordReset(ctx, surface, email)
	}
}
