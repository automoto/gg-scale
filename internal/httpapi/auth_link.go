package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/ggscale/ggscale/internal/auditlog"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/verifycode"
	"github.com/ggscale/ggscale/internal/webutil"
)

// Identity prefixes a Steam link may replace: generated ids from anonymous
// (anon_) and email signup (user_) flows. Developer-keyed custom-token
// identities and existing platform identities are never overwritten.
const (
	anonExternalIDPrefix  = "anon_"
	emailExternalIDPrefix = "user_"
	steamExternalIDPrefix = "steam:"
)

var (
	errAlreadyHasCredentials = errors.New("auth link: player already has credentials")
	errIdentityChanged       = errors.New("auth link: identity changed concurrently")
)

type linkEmailRequest struct {
	Email    string `json:"email,omitempty" example:"player@example.com"`
	Password string `json:"password,omitempty" example:"correct-horse-battery-staple"`
}

type linkEmailInput struct {
	Body linkEmailRequest
}

type linkSteamInput struct {
	Body steamAuthRequest
}

func registerAuthLinkRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "linkEmail",
		Method:      http.MethodPost,
		Path:        "/v1/auth/link",
		Summary:     "Attach email + password sign-in to the calling player",
		Description: "The in-client upgrade path for anonymous players: the player id " +
			"and every save, score, and friend stay untouched. A verification code " +
			"is emailed; after POST /v1/auth/verify the credentials work on any " +
			"device, and verification also links the global gg-scale account " +
			"(unlocking display names and friends). An email another player in the " +
			"project uses is a 409.",
		Tags:          []string{"Authentication"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusAccepted,
	}, authLinkEmail(d))

	huma.Register(api, huma.Operation{
		OperationID: "linkSteam",
		Method:      http.MethodPost,
		Path:        "/v1/auth/link/steam",
		Summary:     "Attach a Steam identity to the calling player",
		Description: "Verifies a Steamworks session ticket (see POST /v1/auth/steam) " +
			"and replaces the caller's generated identity with the Steam identity, " +
			"so later native Steam sign-ins resolve to this player with all data " +
			"intact. Only anonymous or email-signup identities can link; a Steam " +
			"identity another player holds, or a developer-keyed custom-token " +
			"identity, is a 409.",
		Tags:          []string{"Authentication"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusNoContent,
	}, authLinkSteam(d))
}

func authLinkEmail(d Deps) func(context.Context, *linkEmailInput) (*struct{}, error) {
	return func(ctx context.Context, in *linkEmailInput) (*struct{}, error) {
		me, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		if !validateEmail(in.Body.Email) {
			return nil, huma.Error400BadRequest("email invalid")
		}
		if !validPassword(in.Body.Password) {
			return nil, huma.Error400BadRequest("password must be 8–72 characters")
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(in.Body.Password), bcryptCost)
		if err != nil {
			return nil, serverError(ctx, "auth link: hash", err)
		}
		code, err := verifycode.GenerateCode()
		if err != nil {
			return nil, serverError(ctx, "auth link: code", err)
		}
		salt, err := verifycode.NewSalt()
		if err != nil {
			return nil, serverError(ctx, "auth link: salt", err)
		}
		now := apiNow(d)
		email := in.Body.Email

		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			rows, qerr := sqlcgen.New(tx).LinkPlayerEmailCredentials(ctx, sqlcgen.LinkPlayerEmailCredentialsParams{
				Email:                      &email,
				PasswordHash:               hash,
				EmailVerificationCodeHash:  verifycode.Hash(salt, code),
				EmailVerificationSalt:      salt,
				EmailVerificationExpiresAt: pgtype.Timestamptz{Time: now.Add(verifycode.CodeTTL), Valid: true},
				ID:                         me,
			})
			if qerr != nil {
				return qerr
			}
			if rows == 0 {
				return errAlreadyHasCredentials
			}
			return auditlog.Write(ctx, tx, me, "auth.link_email", "", nil)
		})
		switch {
		case errors.Is(err, errAlreadyHasCredentials):
			return nil, huma.Error409Conflict("player already has sign-in credentials")
		case webutil.IsUniqueViolation(err):
			// The plan wants a clear error here (unlike signup's opaque 202):
			// the caller is an authenticated, rate-limited player, not an
			// anonymous prober.
			return nil, huma.Error409Conflict("email already in use by another player")
		case err != nil:
			return nil, serverError(ctx, "auth link: tx", err)
		}

		if d.Mailer != nil {
			_ = d.Mailer.Send(ctx, mailer.Message{
				From: d.MailFrom, To: []string{email},
				Subject: mailerVerifySubject,
				Body:    fmt.Sprintf(mailerVerifyBodyTmpl, code),
			})
		}
		return nil, nil
	}
}

func authLinkSteam(d Deps) func(context.Context, *linkSteamInput) (*struct{}, error) {
	return func(ctx context.Context, in *linkSteamInput) (*struct{}, error) {
		me, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		projectID, _, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}

		var current string
		err = d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
			row, qerr := sqlcgen.New(tx).GetProfile(ctx, me)
			if qerr != nil {
				return qerr
			}
			current = row.ExternalID
			return nil
		})
		if err != nil {
			return nil, serverError(ctx, "steam link: profile", err)
		}
		upgradable := strings.HasPrefix(current, anonExternalIDPrefix) ||
			strings.HasPrefix(current, emailExternalIDPrefix)
		// A developer-keyed identity is rejected before the Valve round-trip;
		// an existing steam: identity still needs the ticket to distinguish
		// an idempotent re-link from a different account.
		if !upgradable && !strings.HasPrefix(current, steamExternalIDPrefix) {
			return nil, huma.Error409Conflict("player identity cannot be replaced")
		}

		res, err := verifySteamTicketForProject(ctx, d, projectID, in.Body.Ticket)
		if err != nil {
			return nil, err
		}
		target := steamExternalIDPrefix + res.SteamID
		if current == target {
			return nil, nil
		}
		if !upgradable {
			return nil, huma.Error409Conflict("player is linked to a different steam account")
		}

		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			rows, qerr := sqlcgen.New(tx).ReplacePlayerExternalID(ctx, sqlcgen.ReplacePlayerExternalIDParams{
				NewExternalID: target,
				ID:            me,
				OldExternalID: current,
			})
			if qerr != nil {
				return qerr
			}
			if rows == 0 {
				return errIdentityChanged
			}
			return auditlog.Write(ctx, tx, me, "auth.link_steam", target, nil)
		})
		switch {
		case errors.Is(err, errIdentityChanged):
			return nil, huma.Error409Conflict("player identity changed, retry")
		case webutil.IsUniqueViolation(err):
			return nil, huma.Error409Conflict("steam account already linked to another player")
		case err != nil:
			return nil, serverError(ctx, "steam link: tx", err)
		}
		return nil, nil
	}
}

// attachVerifiedAccount links a just-verified player to the global account
// layer: find-or-create the player_accounts row for the proven email, then
// bind it. Runs inside the verify transaction. When the bind's own guards
// refuse (the row is already linked to a different account), verification
// still succeeds — the guards exist to protect that older link.
func attachVerifiedAccount(ctx context.Context, q *sqlcgen.Queries, projectID, playerID int64, email string) error {
	acc, err := q.GetPlayerLinkedAccountID(ctx, playerID)
	if err != nil {
		return err
	}
	if acc.Valid {
		return nil
	}
	accID, err := q.FindAccountIDByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		row, gerr := q.GetPlayerByEmail(ctx, sqlcgen.GetPlayerByEmailParams{ProjectID: projectID, Email: &email})
		if gerr != nil {
			return gerr
		}
		// Two-step find-or-create: the DO NOTHING insert never aborts the
		// transaction, and the re-read picks up a concurrent creator's row.
		if err := q.InsertVerifiedPlayerAccountIfAbsent(ctx, sqlcgen.InsertVerifiedPlayerAccountIfAbsentParams{
			Email: email, PasswordHash: row.PasswordHash,
		}); err != nil {
			return err
		}
		accID, err = q.FindAccountIDByEmail(ctx, email)
	}
	if err != nil {
		return err
	}
	_, err = q.BindPlayerLinkedEmail(ctx, sqlcgen.BindPlayerLinkedEmailParams{
		Email: &email, PlayerAccountID: accID, ID: playerID,
	})
	return err
}
