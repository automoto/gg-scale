package projectadmin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/mailer"
	"github.com/automoto/gg-scale/internal/observability"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/verifycode"
	"github.com/automoto/gg-scale/internal/webutil"
)

// Errors of the player writes. Management API clients see their text, so it
// has no package prefix.
var (
	// ErrPlayerNotFound means the player is not in the project, or is gone.
	ErrPlayerNotFound = errors.New("player not found")
	// ErrPlayerDeletePending means a pending deletion owns the disabled state.
	ErrPlayerDeletePending = errors.New("deletion pending; cancel the deletion first")
	// ErrPlayerDeleteRequested means an administrator already requested the deletion.
	ErrPlayerDeleteRequested = errors.New("deletion already requested")
	// ErrNoPendingDelete means the player has no pending deletion to cancel.
	ErrNoPendingDelete = errors.New("no pending deletion")
	// ErrInvalidPlayerEmail means the address of an invite is not valid.
	ErrInvalidPlayerEmail = errors.New("enter a valid email")
	// ErrPlayerEmailTaken means another player of the project has the address.
	ErrPlayerEmailTaken = errors.New("that email is already used by another player in this Game Project")
	// ErrInvitePending means an open invite for the address exists already.
	ErrInvitePending = errors.New("an invite for that email is already pending")
)

// InviteThrottledError means the invite throttle refused the send.
type InviteThrottledError struct {
	// RetryAfter is in seconds, at least 1.
	RetryAfter int
}

func (e *InviteThrottledError) Error() string {
	return "too many invites in a short time; try again in " + strconv.Itoa(e.RetryAfter) + "s"
}

// defaultDeleteGracePeriod is the grace period when none is configured.
const defaultDeleteGracePeriod = 720 * time.Hour

// DeleteGrace returns configured, or the default when it is not set.
func DeleteGrace(configured time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	return defaultDeleteGracePeriod
}

// playerGone returns the error for a guarded write that matched no row: the
// player is either pending deletion (pending) or not in the project.
func playerGone(ctx context.Context, q *sqlcgen.Queries, tenantID, projectID, playerID int64, pending error) error {
	row, err := q.GetPlayerForProject(ctx, sqlcgen.GetPlayerForProjectParams{TenantID: tenantID, ProjectID: projectID, ID: playerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPlayerNotFound
	}
	if err != nil {
		return err
	}
	if row.DeleteRequestedAt.Valid {
		return pending
	}
	return ErrPlayerNotFound
}

// SetPlayerDisabled disables or enables a player in one project and bumps its
// session epoch, so live tokens stop working. at is the disable time.
func SetPlayerDisabled(ctx context.Context, pool *db.Pool, tenantID, projectID, playerID int64, disabled bool, at time.Time, actor Actor) error {
	var disabledAt pgtype.Timestamptz
	action := "control_panel.player.enable"
	if disabled {
		disabledAt = pgtype.Timestamptz{Time: at, Valid: true}
		action = "control_panel.player.disable"
	}
	ctx = db.WithTenant(ctx, tenantID)
	return pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		updated, err := q.SetPlayerDisabledInProject(ctx, sqlcgen.SetPlayerDisabledInProjectParams{
			ID: playerID, ProjectID: projectID, TenantID: tenantID, DisabledAt: disabledAt,
		})
		if err != nil {
			return err
		}
		if updated == 0 {
			// The guarded UPDATE skips players with a pending deletion.
			return playerGone(ctx, q, tenantID, projectID, playerID, ErrPlayerDeletePending)
		}
		return actor.Audit(ctx, tx, tenantID, projectID, action, strconv.FormatInt(playerID, 10), nil)
	})
}

// RequestPlayerDelete disables the player, ends its sessions, and starts the
// grace period. It returns the time the purge sweep may delete the data.
func RequestPlayerDelete(ctx context.Context, pool *db.Pool, tenantID, projectID, playerID int64, grace time.Duration, actor Actor) (time.Time, error) {
	var purgeAt time.Time
	ctx = db.WithTenant(ctx, tenantID)
	err := pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		requestedAt, err := q.RequestPlayerDeleteInProject(ctx, sqlcgen.RequestPlayerDeleteInProjectParams{
			ID: playerID, ProjectID: projectID, TenantID: tenantID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return playerGone(ctx, q, tenantID, projectID, playerID, ErrPlayerDeleteRequested)
		}
		if err != nil {
			return err
		}
		purgeAt = requestedAt.Time.Add(grace)
		if _, err := q.RevokeActivePlayerSessions(ctx, sqlcgen.RevokeActivePlayerSessionsParams{
			ProjectID: projectID, PlayerID: playerID,
		}); err != nil {
			return err
		}
		return actor.Audit(ctx, tx, tenantID, projectID, "control_panel.player.delete_request",
			strconv.FormatInt(playerID, 10), map[string]any{"scheduled_purge_at": purgeAt})
	})
	return purgeAt, err
}

// CancelPlayerDelete ends a pending deletion. It returns ErrNoPendingDelete
// when the player has none.
func CancelPlayerDelete(ctx context.Context, pool *db.Pool, tenantID, projectID, playerID int64, actor Actor) error {
	ctx = db.WithTenant(ctx, tenantID)
	return pool.Q(ctx, func(tx pgx.Tx) error {
		cancelled, err := sqlcgen.New(tx).CancelPlayerDeleteInProject(ctx, sqlcgen.CancelPlayerDeleteInProjectParams{
			ID: playerID, ProjectID: projectID, TenantID: tenantID,
		})
		if err != nil {
			return err
		}
		if cancelled == 0 {
			return ErrNoPendingDelete
		}
		return actor.Audit(ctx, tx, tenantID, projectID, "control_panel.player.delete_cancel", strconv.FormatInt(playerID, 10), nil)
	})
}

const (
	actionPlayerInvite = "control_panel.player.invite"
	actionPlayerLink   = "control_panel.player.link"
	mailNameFallback   = "a game on ggscale"
)

// PlayerInviter sends player invites for the dashboard and for management
// tokens. Throttle may be nil. Mailer may be nil; the invite then exists
// but no email is sent.
type PlayerInviter struct {
	Pool       *db.Pool
	Throttle   *ratelimit.InviteThrottle
	Mailer     mailer.Mailer
	MailFrom   string
	BaseURL    string
	SigningKey []byte
	Metrics    *observability.Metrics
}

// PlayerInvite is the invite that was made.
type PlayerInvite struct {
	ID        int64
	Email     string
	ExpiresAt time.Time
}

// Invite throttles, stores, audits, and emails one player invite. A nil
// targetPlayer makes a plain invite. Otherwise it is a link invite for that
// player: acceptance binds the proven address onto the player, and an open
// invite for the player is replaced. at is the creation time. The inviter
// is the dashboard user, or the creator of the token, so both share one
// throttle. The audit row commits with the invite and holds no address. The
// email goes out after the commit; a send failure is logged and the invite
// stays.
//
// Errors: ErrInvalidPlayerEmail, *InviteThrottledError, ErrProjectNotInTenant,
// ErrPlayerNotFound, ErrPlayerEmailTaken, or a database error.
func (i *PlayerInviter) Invite(ctx context.Context, tenantID, projectID int64, email string, targetPlayer *int64, at time.Time, actor Actor) (PlayerInvite, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if _, err := webutil.ValidateEmail(email); err != nil {
		return PlayerInvite{}, ErrInvalidPlayerEmail
	}
	inviterID := actor.authzUserID()
	attempt := ratelimit.InviteAttempt{
		InviterID: inviterID, TenantID: tenantID, ProjectID: projectID,
		DomainKey: "project:" + strconv.FormatInt(projectID, 10), Recipient: email,
	}
	if retry, throttled := i.throttled(ctx, attempt); throttled {
		return PlayerInvite{}, &InviteThrottledError{RetryAfter: retry}
	}
	res, mail, err := i.create(ctx, tenantID, projectID, email, targetPlayer, at, actor)
	if err != nil {
		// The throttle debited this send, but no invite was made.
		if i.Throttle != nil {
			i.Throttle.Refund(ctx, attempt)
		}
		return PlayerInvite{}, err
	}
	i.Metrics.InviteSent(observability.InvitePlayer)
	i.send(ctx, res, mail, projectID)
	return res, nil
}

// throttled returns the Retry-After seconds and true when the send is
// refused. A throttle error allows the send: a cache fault must not block
// an administrator.
func (i *PlayerInviter) throttled(ctx context.Context, a ratelimit.InviteAttempt) (int, bool) {
	if i.Throttle == nil {
		return 0, false
	}
	dec, err := i.Throttle.Check(ctx, a)
	if err != nil {
		slog.ErrorContext(ctx, "invite throttle check", "err", err)
		return 0, false
	}
	if dec.Allowed {
		return 0, false
	}
	return max(1, int(math.Ceil(dec.RetryAfter.Seconds()))), true
}

// inviteMail is what the email needs besides the invite.
type inviteMail struct {
	code         string
	inviterEmail string
	gameName     string
}

func (i *PlayerInviter) create(ctx context.Context, tenantID, projectID int64, email string, targetPlayer *int64, at time.Time, actor Actor) (PlayerInvite, inviteMail, error) {
	code, err := verifycode.GenerateInviteCode()
	if err != nil {
		return PlayerInvite{}, inviteMail{}, fmt.Errorf("invite code: %w", err)
	}
	expiresAt := at.Add(verifycode.InviteTTL)
	mail := inviteMail{code: code, gameName: mailNameFallback}
	var row sqlcgen.CreatePlayerInvitationRow
	// The invite table is written before a tenant context is set up by the
	// pool, so set app.tenant_id here for row security.
	err = i.Pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", strconv.FormatInt(tenantID, 10)); err != nil {
			return fmt.Errorf("set app.tenant_id: %w", err)
		}
		proj, err := q.GetProjectTenant(ctx, projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProjectNotInTenant
		}
		if err != nil {
			return err
		}
		if proj.TenantID != tenantID {
			return ErrProjectNotInTenant
		}
		mail.gameName = HeaderSafeName(proj.Name, mailNameFallback)
		inviter, err := q.GetControlPanelUserByID(ctx, actor.authzUserID())
		if err != nil {
			return err
		}
		mail.inviterEmail = inviter.Email
		if targetPlayer != nil {
			if err := checkLinkTarget(ctx, q, tenantID, projectID, *targetPlayer, email); err != nil {
				return err
			}
			// Replace an open invite that the unique index would refuse.
			if err := q.RevokeSupersededPlayerInvitations(ctx, sqlcgen.RevokeSupersededPlayerInvitationsParams{
				ProjectID: projectID, ProjectPlayerID: targetPlayer, Email: email,
			}); err != nil {
				return err
			}
		}
		row, err = q.CreatePlayerInvitation(ctx, sqlcgen.CreatePlayerInvitationParams{
			ProjectID:       projectID,
			Email:           email,
			CodeHash:        verifycode.HashToken(code),
			ExpiresAt:       pgtype.Timestamptz{Time: expiresAt, Valid: true},
			InvitedByUserID: actor.authzUserID(),
			ProjectPlayerID: targetPlayer,
		})
		if webutil.IsUniqueViolation(err) {
			return ErrInvitePending
		}
		if err != nil {
			return err
		}
		action, target := actionPlayerInvite, strconv.FormatInt(row.ID, 10)
		if targetPlayer != nil {
			action, target = actionPlayerLink, strconv.FormatInt(*targetPlayer, 10)
		}
		return actor.Audit(ctx, tx, tenantID, projectID, action, target, map[string]any{"invite_id": row.ID})
	})
	if err != nil {
		return PlayerInvite{}, inviteMail{}, err
	}
	return PlayerInvite{ID: row.ID, Email: email, ExpiresAt: row.ExpiresAt.Time}, mail, nil
}

// checkLinkTarget refuses a player that is not in the project, and an
// address that another player of the project has.
func checkLinkTarget(ctx context.Context, q *sqlcgen.Queries, tenantID, projectID, playerID int64, email string) error {
	_, err := q.GetPlayerForProject(ctx, sqlcgen.GetPlayerForProjectParams{TenantID: tenantID, ProjectID: projectID, ID: playerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPlayerNotFound
	}
	if err != nil {
		return err
	}
	existing, err := q.GetPlayerForAccountLink(ctx, sqlcgen.GetPlayerForAccountLinkParams{ProjectID: projectID, Email: &email})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing.ID != playerID {
		return ErrPlayerEmailTaken
	}
	return nil
}

// send mails the recipient a link into the player site. A failure is
// logged and does not undo the invite. An unsubscribed address gets no mail.
func (i *PlayerInviter) send(ctx context.Context, res PlayerInvite, m inviteMail, projectID int64) {
	if i.Mailer == nil || i.MailFrom == "" {
		slog.WarnContext(ctx, "player invite: no mailer configured", "invite_id", res.ID, "domain", emailDomain(res.Email))
		return
	}
	if EmailSuppressed(ctx, i.Pool, res.Email) {
		return
	}
	link := strings.TrimRight(i.BaseURL, "/") + "/v1/players/p/" + strconv.FormatInt(projectID, 10) + "/invite/accept?code=" + url.QueryEscape(m.code)
	unsubscribe := webutil.EmailUnsubscribeURL(i.BaseURL, i.SigningKey, res.Email)
	body := fmt.Sprintf(
		"%s invited you to play %s.\n\n"+
			"Accept the invite and set up your player account here:\n%s\n\n"+
			"ggscale is the player-account service %s uses for sign-in, saves, friends, and leaderboards. "+
			"One ggscale account works across every game built on it.\n\n"+
			"This invite expires %s.\n\n"+
			"Don't want invite emails? Unsubscribe: %s",
		m.inviterEmail, m.gameName, link, m.gameName,
		res.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"), unsubscribe)
	if err := i.Mailer.Send(ctx, mailer.Message{
		From:            i.MailFrom,
		To:              []string{res.Email},
		Subject:         "You're invited to play " + m.gameName,
		Body:            body,
		ListUnsubscribe: unsubscribe,
	}); err != nil {
		// The mailer error holds the SMTP step and reply code, not the address.
		slog.ErrorContext(ctx, "player invite mailer", "err", err, "invite_id", res.ID, "domain", emailDomain(res.Email))
	}
}

// HeaderSafeName returns name when it can go into a mail subject: not empty,
// at most 120 bytes, and with no control characters. Otherwise it returns
// fallback.
func HeaderSafeName(name, fallback string) string {
	const maxNameBytes = 120
	if name == "" || len(name) > maxNameBytes {
		return fallback
	}
	if _, err := webutil.SanitizeHeader(name); err != nil {
		return fallback
	}
	return name
}

// EmailSuppressed reports whether the platform-wide unsubscribe list blocks
// invite mail to email. It fails closed: on a lookup error it returns true.
func EmailSuppressed(ctx context.Context, pool *db.Pool, email string) bool {
	var suppressed bool
	err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		suppressed, qerr = sqlcgen.New(tx).IsEmailSuppressed(ctx, strings.ToLower(strings.TrimSpace(email)))
		return qerr
	})
	if err != nil {
		slog.ErrorContext(ctx, "invite suppression lookup", "err", err)
		return true
	}
	if suppressed {
		slog.InfoContext(ctx, "invite email dropped: address unsubscribed", "domain", emailDomain(email))
	}
	return suppressed
}

// emailDomain returns the part after the last "@", for logs that must not
// hold the full address.
func emailDomain(email string) string {
	i := strings.LastIndex(email, "@")
	if i < 0 {
		return ""
	}
	return email[i+1:]
}
