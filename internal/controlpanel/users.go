package controlpanel

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

const (
	roleMember = "member"
	roleAdmin  = "admin"
	roleOwner  = "owner"
)

var (
	errInvalidCredentials = errors.New("control panel: invalid credentials")
	errLockedAccount      = errors.New("control panel: account locked")
)

type contextKey string

const sessionContextKey contextKey = "control-panel-session"

type controlPanelUser struct {
	ID              int64
	Email           string
	IsPlatformAdmin bool
}

type controlPanelSession struct {
	ID        int64
	User      controlPanelUser
	CSRFToken string
	ExpiresAt time.Time
}

func contextWithSession(ctx context.Context, session controlPanelSession) context.Context {
	return context.WithValue(ctx, sessionContextKey, session)
}

func sessionFromContext(ctx context.Context) (controlPanelSession, bool) {
	session, ok := ctx.Value(sessionContextKey).(controlPanelSession)
	return session, ok
}

func (h *Handler) controlPanelQ(ctx context.Context, controlPanelUserID int64, fn func(pgx.Tx) error) error {
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.control_panel_user_id', $1, true)", stringFromInt(controlPanelUserID)); err != nil {
			return fmt.Errorf("set app.control_panel_user_id: %w", err)
		}
		return fn(tx)
	})
}

func (h *Handler) listTenants(ctx context.Context, user controlPanelUser) ([]TenantView, error) {
	var tenants []TenantView
	err := h.controlPanelQ(ctx, user.ID, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if user.IsPlatformAdmin {
			rows, err := q.ListControlPanelTenantsForPlatformAdmin(ctx)
			if err != nil {
				return fmt.Errorf("list platform tenants: %w", err)
			}
			tenants = make([]TenantView, 0, len(rows))
			for _, row := range rows {
				tenants = append(tenants, tenantView(row.ID, row.Name, row.Role, row.CreatedAt.Time))
			}
			return nil
		}

		rows, err := q.ListControlPanelTenantsForUser(ctx, user.ID)
		if err != nil {
			return fmt.Errorf("list user tenants: %w", err)
		}
		tenants = make([]TenantView, 0, len(rows))
		for _, row := range rows {
			tenants = append(tenants, tenantView(row.ID, row.Name, row.Role, row.CreatedAt.Time))
		}
		return nil
	})
	return tenants, err
}

func tenantView(id int64, name, role string, createdAt time.Time) TenantView {
	return TenantView{ID: id, Name: name, Role: role, CreatedAt: createdAt}
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func validControlPanelEmail(email string) bool {
	_, err := mail.ParseAddress(email)
	return err == nil
}
