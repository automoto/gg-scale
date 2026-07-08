package controlpanel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ggscale/ggscale/internal/auditlog"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/verifycode"
)

var (
	errInvalidInviteEmail = errors.New("control panel: invalid invite email")
	errInvalidInviteRole  = errors.New("control panel: invalid invite role")
	errInviteExists       = errors.New("control panel: open invite exists")
	errInviteNotFound     = errors.New("control panel: invite not found")
	errInviteExpired      = errors.New("control panel: invite expired")
	errCannotRemoveSelf   = errors.New("control panel: cannot remove yourself")
	errInvalidGrantRole   = errors.New("control panel: role is not grantable")
	errMemberNotInTenant  = errors.New("control panel: user is not a member of this tenant")
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
	if !validControlPanelEmail(normalizeEmail(in.Email)) {
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

	var row sqlcgen.CreateControlPanelInvitationRow
	err = h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		r, qerr := sqlcgen.New(tx).CreateControlPanelInvitation(ctx, sqlcgen.CreateControlPanelInvitationParams{
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
		return auditlog.WritePlatform(ctx, tx, in.InvitedBy, "control_panel.invite.create", strconv.FormatInt(r.ID, 10), map[string]any{
			"email": email,
			"role":  in.Role,
		})
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
		mrows, err := q.ListControlPanelMembersForTenant(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("list members: %w", err)
		}
		for _, m := range mrows {
			member := TeamMemberView{
				MembershipID:    m.MembershipID,
				UserID:          m.UserID,
				Email:           m.Email,
				Role:            m.Role,
				IsPlatformAdmin: m.IsPlatformAdmin,
				LastLoginAt:     m.LastLoginAt.Time,
				JoinedAt:        m.CreatedAt.Time,
			}
			if h.rbac != nil {
				member.FleetOperator, _ = h.rbac.HasControlPanelRole(m.UserID, tenantID, rbac.RoleFleetOperator)
			}
			members = append(members, member)
		}
		prows, err := q.ListControlPanelInvitationsForTenant(ctx, &tenantID)
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
func (h *Handler) revokeInvite(ctx context.Context, actorID, inviteID int64) error {
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		if err := sqlcgen.New(tx).RevokeControlPanelInvitation(ctx, inviteID); err != nil {
			return err
		}
		return auditlog.WritePlatform(ctx, tx, actorID, "control_panel.invite.revoke", strconv.FormatInt(inviteID, 10), nil)
	})
}

// removeMember deletes a tenant membership. Refuses to remove the actor's
// own row — the SQL WHERE clause folds the self-check into a single
// statement so we don't have to scan every member row to enforce it.
func (h *Handler) removeMember(ctx context.Context, actorID int64, tenantID, membershipID int64) error {
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var targetUserID int64
		err := tx.QueryRow(ctx, `
SELECT control_panel_user_id
FROM control_panel_memberships
WHERE id = $1
  AND tenant_id = $2
  AND control_panel_user_id <> $3`,
			membershipID, tenantID, actorID).Scan(&targetUserID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errCannotRemoveSelf
		}
		if err != nil {
			return err
		}
		if h.rbac != nil {
			if err := h.rbac.RemoveControlPanelRolesTx(ctx, tx, targetUserID, tenantID); err != nil {
				return fmt.Errorf("rbac remove membership: %w", err)
			}
		}
		n, err := q.DeleteControlPanelMembershipUnlessSelf(ctx, sqlcgen.DeleteControlPanelMembershipUnlessSelfParams{
			ID:          membershipID,
			TenantID:    tenantID,
			ActorUserID: actorID,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			// Either the row doesn't exist or it's the actor's own — the
			// caller treats both as "refused". GetControlPanelMembership
			// would disambiguate but the control panel surfaces either with
			// the same message.
			return errCannotRemoveSelf
		}
		return auditlog.WritePlatform(ctx, tx, actorID, "control_panel.membership.remove", strconv.FormatInt(membershipID, 10), map[string]any{"tenant_id": tenantID})
	})
	if err != nil {
		return err
	}
	h.reloadRBACPolicy(ctx)
	return nil
}

// setTeamMemberRole grants or revokes an à-la-carte control panel role (e.g.
// fleet_operator) on a tenant member, alongside their membership role. Only
// roles in rbac.GrantableControlPanelRole are accepted; the target must be an
// existing member of the tenant.
func (h *Handler) setTeamMemberRole(ctx context.Context, actorID, tenantID, targetUserID int64, role string, grant bool) error {
	if !rbac.GrantableControlPanelRole(role) {
		return errInvalidGrantRole
	}
	if h.rbac == nil {
		return errors.New(msgControlPanelPoolNeeded)
	}
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM control_panel_memberships WHERE tenant_id = $1 AND control_panel_user_id = $2)`,
			tenantID, targetUserID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errMemberNotInTenant
		}
		if grant {
			if err := h.rbac.AddControlPanelRoleTx(ctx, tx, targetUserID, tenantID, role); err != nil {
				return fmt.Errorf("rbac grant role: %w", err)
			}
		} else if err := h.rbac.RemoveControlPanelRoleTx(ctx, tx, targetUserID, tenantID, role); err != nil {
			return fmt.Errorf("rbac revoke role: %w", err)
		}
		action := "control_panel.member.role_revoke"
		if grant {
			action = "control_panel.member.role_grant"
		}
		return auditlog.WritePlatform(ctx, tx, actorID, action, strconv.FormatInt(targetUserID, 10), map[string]any{
			"role":      role,
			"tenant_id": tenantID,
		})
	})
	if err != nil {
		return err
	}
	h.reloadRBACPolicy(ctx)
	return nil
}
