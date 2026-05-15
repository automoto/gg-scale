package dashboard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/verifycode"
)

var (
	errInvalidInviteEmail = errors.New("dashboard: invalid invite email")
	errInvalidInviteRole  = errors.New("dashboard: invalid invite role")
	errInviteExists       = errors.New("dashboard: open invite exists")
	errInviteNotFound     = errors.New("dashboard: invite not found")
	errInviteExpired      = errors.New("dashboard: invite expired")
	errCannotRemoveSelf   = errors.New("dashboard: cannot remove yourself")
)

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return strings.Contains(err.Error(), "23505")
}

// inviteTeammateInput is the validated input for the create-invite handler.
type inviteTeammateInput struct {
	Email     string
	Role      string
	TenantID  *int64
	InvitedBy int64
}

type inviteResult struct {
	ID        int64
	Email     string
	Role      string
	Code      string
	ExpiresAt time.Time
}

// validateInviteInput checks the role/tenant/email triple without DB I/O so
// it can be exercised by unit tests.
func validateInviteInput(in inviteTeammateInput) error {
	if !validDashboardEmail(normalizeEmail(in.Email)) {
		return errInvalidInviteEmail
	}
	if !validInviteRole(in.Role) {
		return errInvalidInviteRole
	}
	if in.Role == roleInvitePlatformAdmin && in.TenantID != nil {
		return errInvalidInviteRole
	}
	if in.Role != roleInvitePlatformAdmin && in.TenantID == nil {
		return errInvalidInviteRole
	}
	return nil
}

// createInvite mints a new code, persists the row, and returns the
// plaintext code so the caller can email it. The plaintext is never stored.
func (h *Handler) createInvite(ctx context.Context, in inviteTeammateInput) (inviteResult, error) {
	if err := validateInviteInput(in); err != nil {
		return inviteResult{}, err
	}
	email := normalizeEmail(in.Email)

	code, err := verifycode.GenerateInviteCode()
	if err != nil {
		return inviteResult{}, fmt.Errorf("invite code: %w", err)
	}
	codeHash := verifycode.Hash(nil, code)
	expiresAt := h.now().Add(verifycode.InviteTTL)

	var row sqlcgen.CreateDashboardInvitationRow
	err = h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		r, qerr := sqlcgen.New(tx).CreateDashboardInvitation(ctx, sqlcgen.CreateDashboardInvitationParams{
			Email:           email,
			TenantID:        in.TenantID,
			Role:            in.Role,
			CodeHash:        codeHash,
			ExpiresAt:       pgtype.Timestamptz{Time: expiresAt, Valid: true},
			InvitedByUserID: in.InvitedBy,
		})
		if qerr != nil {
			return qerr
		}
		row = r
		return nil
	})
	if err != nil {
		if isUniqueViolation(err) {
			return inviteResult{}, errInviteExists
		}
		return inviteResult{}, err
	}
	return inviteResult{
		ID:        row.ID,
		Email:     email,
		Role:      in.Role,
		Code:      code,
		ExpiresAt: row.ExpiresAt.Time,
	}, nil
}

// listTenantTeam returns members + pending invites for a tenant.
func (h *Handler) listTenantTeam(ctx context.Context, tenantID int64) ([]TeamMemberView, []PendingInviteView, error) {
	var (
		members []TeamMemberView
		pending []PendingInviteView
	)
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		mrows, err := q.ListDashboardMembersForTenant(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("list members: %w", err)
		}
		for _, m := range mrows {
			members = append(members, TeamMemberView{
				MembershipID:    m.MembershipID,
				UserID:          m.UserID,
				Email:           m.Email,
				Role:            m.Role,
				IsPlatformAdmin: m.IsPlatformAdmin,
				LastLoginAt:     m.LastLoginAt.Time,
				JoinedAt:        m.CreatedAt.Time,
			})
		}
		prows, err := q.ListDashboardInvitationsForTenant(ctx, &tenantID)
		if err != nil {
			return fmt.Errorf("list invites: %w", err)
		}
		for _, p := range prows {
			pending = append(pending, PendingInviteView{
				ID:          p.ID,
				Email:       p.Email,
				Role:        p.Role,
				InvitedByID: p.InvitedByUserID,
				ExpiresAt:   p.ExpiresAt.Time,
				CreatedAt:   p.CreatedAt.Time,
			})
		}
		return nil
	})
	return members, pending, err
}

// listPlatformTeam returns platform admins + pending platform admin invites.
func (h *Handler) listPlatformTeam(ctx context.Context) ([]TeamMemberView, []PendingInviteView, error) {
	var (
		admins  []TeamMemberView
		pending []PendingInviteView
	)
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		arows, err := q.ListPlatformAdmins(ctx)
		if err != nil {
			return fmt.Errorf("list platform admins: %w", err)
		}
		for _, a := range arows {
			admins = append(admins, TeamMemberView{
				UserID:          a.UserID,
				Email:           a.Email,
				Role:            roleInvitePlatformAdmin,
				IsPlatformAdmin: true,
				LastLoginAt:     a.LastLoginAt.Time,
				JoinedAt:        a.CreatedAt.Time,
			})
		}
		prows, err := q.ListPlatformAdminInvitations(ctx)
		if err != nil {
			return fmt.Errorf("list platform invites: %w", err)
		}
		for _, p := range prows {
			pending = append(pending, PendingInviteView{
				ID:          p.ID,
				Email:       p.Email,
				Role:        p.Role,
				InvitedByID: p.InvitedByUserID,
				ExpiresAt:   p.ExpiresAt.Time,
				CreatedAt:   p.CreatedAt.Time,
			})
		}
		return nil
	})
	return admins, pending, err
}

// revokeInvite marks an invite revoked. Caller must have already verified
// that the actor is allowed to revoke this invite (platform admin always,
// tenant admin only if the invite's tenant matches).
func (h *Handler) revokeInvite(ctx context.Context, inviteID int64) error {
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).RevokeDashboardInvitation(ctx, inviteID)
	})
}

// removeMember deletes a tenant membership. Refuses to remove the actor
// from their own tenant (use revoke + recreate or have another admin do it).
func (h *Handler) removeMember(ctx context.Context, actorID int64, tenantID, membershipID int64) error {
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		// Look up the membership to find the user so we can reject self-removal.
		rows, err := q.ListDashboardMembersForTenant(ctx, tenantID)
		if err != nil {
			return err
		}
		for _, m := range rows {
			if m.MembershipID == membershipID && m.UserID == actorID {
				return errCannotRemoveSelf
			}
		}
		return q.DeleteDashboardMembership(ctx, sqlcgen.DeleteDashboardMembershipParams{
			ID:       membershipID,
			TenantID: tenantID,
		})
	})
}
