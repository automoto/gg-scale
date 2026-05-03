package dashboard

import (
	"context"
	"errors"
	"fmt"
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
	errInvalidCredentials = errors.New("dashboard: invalid credentials")
	errLockedAccount      = errors.New("dashboard: account locked")
)

type contextKey string

const sessionContextKey contextKey = "dashboard-session"

type dashboardUser struct {
	ID              int64
	Email           string
	IsPlatformAdmin bool
}

type dashboardSession struct {
	ID        int64
	User      dashboardUser
	CSRFToken string
	ExpiresAt time.Time
}

func contextWithSession(ctx context.Context, session dashboardSession) context.Context {
	return context.WithValue(ctx, sessionContextKey, session)
}

func sessionFromContext(ctx context.Context) (dashboardSession, bool) {
	session, ok := ctx.Value(sessionContextKey).(dashboardSession)
	return session, ok
}

func (h *Handler) dashboardQ(ctx context.Context, dashboardUserID int64, fn func(pgx.Tx) error) error {
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.dashboard_user_id', $1, true)", stringFromInt(dashboardUserID)); err != nil {
			return fmt.Errorf("set app.dashboard_user_id: %w", err)
		}
		return fn(tx)
	})
}

func (h *Handler) listTenants(ctx context.Context, user dashboardUser) ([]TenantView, error) {
	var tenants []TenantView
	err := h.dashboardQ(ctx, user.ID, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if user.IsPlatformAdmin {
			rows, err := q.ListDashboardTenantsForPlatformAdmin(ctx)
			if err != nil {
				return fmt.Errorf("list platform tenants: %w", err)
			}
			tenants = make([]TenantView, 0, len(rows))
			for _, row := range rows {
				tenants = append(tenants, tenantView(row.ID, row.Name, row.Role, row.CreatedAt.Time))
			}
			return nil
		}

		rows, err := q.ListDashboardTenantsForUser(ctx, user.ID)
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

func (h *Handler) userCanAccessTenant(ctx context.Context, user dashboardUser, tenantID int64, minRole string) (bool, error) {
	if user.IsPlatformAdmin {
		return true, nil
	}
	var role string
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).GetDashboardMembership(ctx, sqlcgen.GetDashboardMembershipParams{
			DashboardUserID: user.ID,
			TenantID:        tenantID,
		})
		if err != nil {
			return err
		}
		role = row.Role
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return roleAllows(role, minRole), nil
}

func roleAllows(role, minRole string) bool {
	rank := func(role string) int {
		switch role {
		case roleOwner:
			return 3
		case roleAdmin:
			return 2
		case roleMember:
			return 1
		default:
			return 0
		}
	}
	return rank(role) >= rank(minRole)
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func validDashboardEmail(email string) bool {
	return len(email) >= 5 && strings.Contains(email, "@") && strings.Contains(email[strings.LastIndex(email, "@"):], ".")
}
