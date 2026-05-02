package middleware

import (
	"context"
	"log/slog"

	"github.com/ggscale/ggscale/internal/db"
)

// ContextHandler is a slog.Handler that enriches every log record with
// tenant_id, project_id (when set), and request_id read from the context.
// Wrap the default JSON handler in main.go so structured logs always carry
// request-scoped identifiers without call sites having to add them manually.
type ContextHandler struct {
	next slog.Handler
}

// NewContextHandler wraps next so every Handle call auto-injects
// tenant_id, project_id, and request_id from the context.
func NewContextHandler(next slog.Handler) *ContextHandler {
	return &ContextHandler{next: next}
}

// Enabled delegates to the underlying handler.
func (h *ContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle enriches r with context values before passing it downstream.
func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if tid, err := db.TenantFromContext(ctx); err == nil {
		r.AddAttrs(slog.Int64("tenant_id", tid))
	}
	if pid, ok := db.ProjectFromContext(ctx); ok {
		r.AddAttrs(slog.Int64("project_id", pid))
	}
	if rid := RequestIDFromContext(ctx); rid != "" {
		r.AddAttrs(slog.String("request_id", rid))
	}
	return h.next.Handle(ctx, r)
}

// WithAttrs returns a new ContextHandler whose underlying handler has the
// given attributes pre-applied.
func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{next: h.next.WithAttrs(attrs)}
}

// WithGroup returns a new ContextHandler scoped to name.
func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{next: h.next.WithGroup(name)}
}
