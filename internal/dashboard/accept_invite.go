package dashboard

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
// and resolves whether the invitee already has a dashboard_users row.
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
		row, qerr := q.GetDashboardInvitationByCodeHash(ctx, codeHash)
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
		existing, gerr := q.GetDashboardUserAnyStatusByEmail(ctx, row.Email)
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

// acceptInvite resolves the invite, creates or updates the dashboard_user,
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
		row, qerr := q.GetDashboardInvitationByCodeHash(ctx, codeHash)
		if errors.Is(qerr, pgx.ErrNoRows) {
			return errInviteNotFound
		}
		if qerr != nil {
			return qerr
		}
		if verifycode.Expired(row.ExpiresAt.Time, h.now()) {
			return errInviteExpired
		}

		user, gerr := q.GetDashboardUserAnyStatusByEmail(ctx, row.Email)
		switch {
		case errors.Is(gerr, pgx.ErrNoRows):
			if len(in.Password) < 12 {
				return errWeakPassword
			}
			pwHash, hashErr := bcrypt.GenerateFromPassword([]byte(in.Password), bcryptCost)
			if hashErr != nil {
				return fmt.Errorf("invite bcrypt: %w", hashErr)
			}
			created, cerr := q.CreateVerifiedDashboardUser(ctx, sqlcgen.CreateVerifiedDashboardUserParams{
				Email:           row.Email,
				PasswordHash:    pwHash,
				IsPlatformAdmin: row.Role == roleInvitePlatformAdmin,
			})
			if cerr != nil {
				return fmt.Errorf("invite create user: %w", cerr)
			}
			out.UserID = created.ID
			out.Email = created.Email
			out.IsNewUser = true
		case gerr != nil:
			return gerr
		case user.DisabledAt.Valid:
			// User exists but a platform admin disabled them; refuse
			// the invite acceptance with a friendly sentinel.
			return errInviteForDisabledAccount
		default:
			// Existing user: require the CURRENT password before granting
			// a session or escalating roles. Mere possession of the magic
			// link is otherwise a full account takeover for anyone with
			// inbox / mail-server access.
			if bcrypt.CompareHashAndPassword(user.PasswordHash, []byte(in.Password)) != nil {
				return errInvalidCredentials
			}
			out.UserID = user.ID
			out.Email = user.Email
			if row.Role == roleInvitePlatformAdmin && !user.IsPlatformAdmin {
				if err := q.PromoteDashboardUserToPlatformAdmin(ctx, user.ID); err != nil {
					return fmt.Errorf("invite promote: %w", err)
				}
			}
		}

		if row.TenantID != nil {
			membershipRole := roleAdmin
			if row.Role == roleInviteTenantMember {
				membershipRole = roleMember
			}
			if _, err := q.CreateDashboardMembership(ctx, sqlcgen.CreateDashboardMembershipParams{
				DashboardUserID: out.UserID,
				TenantID:        *row.TenantID,
				Role:            membershipRole,
			}); err != nil {
				return fmt.Errorf("invite membership: %w", err)
			}
		}

		return q.MarkDashboardInvitationAccepted(ctx, row.ID)
	})
	return out, err
}

var errWeakPassword = errors.New("dashboard: password too short")

// errInviteForDisabledAccount is returned when an invite-accept request
// targets an email whose dashboard_users row has disabled_at set. The
// handler renders a 403 with a "this account is disabled" message.
var errInviteForDisabledAccount = errors.New("dashboard: invite for disabled account")
