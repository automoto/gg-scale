package controlpanel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/verifycode"
)

// lookupInviteResult is the public view of an invitation while it is being
// inspected (e.g. on the magic-link landing page). The plaintext code is
// never stored; it lives only in the URL the invitee clicked.
type lookupInviteResult struct {
	InviteID   int64
	Email      string
	Role       string
	TenantID   *int64
	TenantName string
	ExpiresAt  time.Time
	IsExisting bool
}

// lookupInvite hashes the URL code, finds the matching open invitation,
// and resolves whether the invitee already has a control_panel_users row.
// Returns errInviteNotFound for any non-acceptance state so callers can
// show a generic message without leaking which case occurred.
func (h *Handler) lookupInvite(ctx context.Context, code string) (lookupInviteResult, error) {
	if code == "" {
		return lookupInviteResult{}, errInviteNotFound
	}
	codeHash := verifycode.Hash(nil, code)

	var out lookupInviteResult
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		row, qerr := q.GetControlPanelInvitationByCodeHash(ctx, codeHash)
		if errors.Is(qerr, pgx.ErrNoRows) {
			return errInviteNotFound
		}
		if qerr != nil {
			return qerr
		}
		out.InviteID = row.ID
		out.Email = row.Email
		out.Role = row.Role
		out.ExpiresAt = row.ExpiresAt.Time
		if row.TenantID != nil {
			tid := *row.TenantID
			out.TenantID = &tid
			if row.TenantName != nil {
				out.TenantName = *row.TenantName
			}
		}
		// Status-blind lookup: distinguishes "no user yet" (new-user
		// branch later) from "user exists but disabled" (refuse).
		existing, gerr := q.GetControlPanelUserAnyStatusByEmail(ctx, row.Email)
		switch {
		case errors.Is(gerr, pgx.ErrNoRows):
			return nil
		case gerr != nil:
			return gerr
		case existing.DisabledAt.Valid:
			return errInviteForDisabledAccount
		default:
			out.IsExisting = true
			return nil
		}
	})
	if err != nil {
		return lookupInviteResult{}, err
	}
	if verifycode.Expired(out.ExpiresAt, h.now()) {
		return lookupInviteResult{}, errInviteExpired
	}
	return out, nil
}

// acceptInviteInput is the validated input for the accept handler.
//
// For NEW invitees `Password` is the password they want to set. For
// EXISTING invitees `Password` is their CURRENT password — required so
// that mere possession of the magic link is not enough to take over the
// account.
type acceptInviteInput struct {
	Code     string
	Password string
}

type acceptInviteResult struct {
	UserID    int64
	Email     string
	IsNewUser bool
}

// acceptInvite resolves the invite, creates or updates the control_panel_user,
// writes the membership row (or promotes platform admin), and marks the
// invite accepted in a single transaction.
func (h *Handler) acceptInvite(ctx context.Context, in acceptInviteInput) (acceptInviteResult, error) {
	if in.Code == "" {
		return acceptInviteResult{}, errInviteNotFound
	}
	codeHash := verifycode.Hash(nil, in.Code)

	var out acceptInviteResult
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		row, qerr := q.GetControlPanelInvitationByCodeHash(ctx, codeHash)
		if errors.Is(qerr, pgx.ErrNoRows) {
			return errInviteNotFound
		}
		if qerr != nil {
			return qerr
		}
		if verifycode.Expired(row.ExpiresAt.Time, h.now()) {
			return errInviteExpired
		}

		resolved, rerr := h.resolveInviteUser(ctx, q, row, in)
		if rerr != nil {
			return rerr
		}
		out = resolved

		if err := h.applyInviteRoles(ctx, tx, q, row, out.UserID); err != nil {
			return err
		}
		return q.MarkControlPanelInvitationAccepted(ctx, row.ID)
	})
	if err != nil {
		return out, err
	}
	h.reloadRBACPolicy(ctx)
	return out, nil
}

// resolveInviteUser returns the control panel user the invite resolves to,
// creating a verified account for first-time invitees and verifying the
// current password (and promoting platform admins) for existing ones.
func (h *Handler) resolveInviteUser(ctx context.Context, q *sqlcgen.Queries, row sqlcgen.GetControlPanelInvitationByCodeHashRow, in acceptInviteInput) (acceptInviteResult, error) {
	user, gerr := q.GetControlPanelUserAnyStatusByEmail(ctx, row.Email)
	switch {
	case errors.Is(gerr, pgx.ErrNoRows):
		return h.createInviteUser(ctx, q, row, in)
	case gerr != nil:
		return acceptInviteResult{}, gerr
	case user.DisabledAt.Valid:
		// User exists but a platform admin disabled them; refuse
		// the invite acceptance with a friendly sentinel.
		return acceptInviteResult{}, errInviteForDisabledAccount
	}

	// Existing user: require the CURRENT password before granting a session
	// or escalating roles. Mere possession of the magic link is otherwise a
	// full account takeover for anyone with inbox / mail-server access.
	if bcrypt.CompareHashAndPassword(user.PasswordHash, []byte(in.Password)) != nil {
		return acceptInviteResult{}, errInvalidCredentials
	}
	if row.Role == roleInvitePlatformAdmin && !user.IsPlatformAdmin {
		if err := q.PromoteControlPanelUserToPlatformAdmin(ctx, user.ID); err != nil {
			return acceptInviteResult{}, fmt.Errorf("invite promote: %w", err)
		}
	}
	return acceptInviteResult{UserID: user.ID, Email: user.Email}, nil
}

// createInviteUser provisions a verified control_panel_user for a first-time
// invitee, enforcing the password floor.
func (h *Handler) createInviteUser(ctx context.Context, q *sqlcgen.Queries, row sqlcgen.GetControlPanelInvitationByCodeHashRow, in acceptInviteInput) (acceptInviteResult, error) {
	if len(in.Password) < minControlPanelPassLen {
		return acceptInviteResult{}, errWeakPassword
	}
	pwHash, hashErr := bcrypt.GenerateFromPassword([]byte(in.Password), bcryptCost)
	if hashErr != nil {
		return acceptInviteResult{}, fmt.Errorf("invite bcrypt: %w", hashErr)
	}
	created, cerr := q.CreateVerifiedControlPanelUser(ctx, sqlcgen.CreateVerifiedControlPanelUserParams{
		Email:           row.Email,
		PasswordHash:    pwHash,
		IsPlatformAdmin: row.Role == roleInvitePlatformAdmin,
	})
	if cerr != nil {
		return acceptInviteResult{}, fmt.Errorf("invite create user: %w", cerr)
	}
	return acceptInviteResult{UserID: created.ID, Email: created.Email, IsNewUser: true}, nil
}

// applyInviteRoles writes the platform-admin and tenant-membership grants the
// invite confers, mirroring each into the RBAC store.
func (h *Handler) applyInviteRoles(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, row sqlcgen.GetControlPanelInvitationByCodeHashRow, userID int64) error {
	if h.rbac != nil && row.Role == roleInvitePlatformAdmin {
		if err := h.rbac.AddPlatformAdminTx(ctx, tx, userID); err != nil {
			return fmt.Errorf("rbac platform admin invite: %w", err)
		}
	}
	if row.TenantID == nil {
		return nil
	}

	membershipRole := roleAdmin
	if row.Role == roleInviteTenantMember {
		membershipRole = roleMember
	}
	if _, err := q.CreateControlPanelMembership(ctx, sqlcgen.CreateControlPanelMembershipParams{
		ControlPanelUserID: userID,
		TenantID:           *row.TenantID,
		Role:               membershipRole,
	}); err != nil {
		return fmt.Errorf("invite membership: %w", err)
	}
	if h.rbac != nil {
		if err := h.rbac.SetControlPanelMembershipRoleTx(ctx, tx, userID, *row.TenantID, membershipRole); err != nil {
			return fmt.Errorf("rbac tenant invite: %w", err)
		}
	}
	return nil
}

var errWeakPassword = errors.New("control panel: password too short")

// errInviteForDisabledAccount is returned when an invite-accept request
// targets an email whose control_panel_users row has disabled_at set. The
// handler renders a 403 with a "this account is disabled" message.
var errInviteForDisabledAccount = errors.New("control panel: invite for disabled account")
