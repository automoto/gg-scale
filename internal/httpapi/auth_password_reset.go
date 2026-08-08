package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/automoto/gg-scale/internal/auditlog"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/mailer"
	"github.com/automoto/gg-scale/internal/observability"
	"github.com/automoto/gg-scale/internal/verifycode"
)

const (
	mailerResetSubject  = "Reset your ggscale password"
	mailerResetBodyTmpl = "Your ggscale password reset code is %s (valid 15 minutes)."
)

type resetRequestInput struct {
	Body struct {
		Email string `json:"email,omitempty" example:"player@example.com"`
	}
}

type resetConfirmInput struct {
	Body struct {
		Email       string `json:"email,omitempty" example:"player@example.com"`
		Code        string `json:"code,omitempty" example:"483920"`
		NewPassword string `json:"new_password,omitempty" example:"correct-horse-battery-staple"`
	}
}

type resendVerifyInput struct {
	Body struct {
		Email string `json:"email,omitempty" example:"player@example.com"`
	}
}

// registerAuthResetRoutes registers the email-taking, code-sending endpoints.
// Called from registerAuthPasswordRoutes so they share the fixed per-IP
// limiter with signup/login. Every response is opaque: the same status
// whether or not the email exists.
func registerAuthResetRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "requestPasswordReset",
		Method:      http.MethodPost,
		Path:        "/v1/auth/password-reset",
		Summary:     "Request an in-client password reset code",
		Description: "Emails a reset code when the address belongs to a player of the " +
			"project. Always answers 202 — the response never reveals whether an " +
			"account exists. Repeat requests inside a one-minute cooldown send " +
			"nothing. Finish with POST /v1/auth/password-reset/confirm.",
		Tags:          []string{"Authentication"},
		Security:      apiKeySecurity,
		DefaultStatus: http.StatusAccepted,
	}, resetRequest(d))

	huma.Register(api, huma.Operation{
		OperationID: "confirmPasswordReset",
		Method:      http.MethodPost,
		Path:        "/v1/auth/password-reset/confirm",
		Summary:     "Confirm a password reset with the emailed code",
		Description: "Sets the new password and revokes every existing session and " +
			"refresh token for the player. Wrong codes are capped per code and " +
			"per lifetime; a rejected new password does not consume the code.",
		Tags:          []string{"Authentication"},
		Security:      apiKeySecurity,
		DefaultStatus: http.StatusNoContent,
	}, resetConfirm(d))

	huma.Register(api, huma.Operation{
		OperationID: "resendVerification",
		Method:      http.MethodPost,
		Path:        "/v1/auth/verify/resend",
		Summary:     "Resend the email verification code",
		Description: "Mints a fresh code (replacing the old one) when the address " +
			"belongs to an unverified player, subject to a one-minute cooldown. " +
			"Always answers 202. Signing up again with an existing email also " +
			"answers 202 and sends a sign-in notice instead of a duplicate " +
			"account.",
		Tags:          []string{"Authentication"},
		Security:      apiKeySecurity,
		DefaultStatus: http.StatusAccepted,
	}, resendVerify(d))
}

// mintAndSendCode generates a challenge code + salt, runs prepare inside one
// transaction, and mails the code when prepare reports the caller eligible.
// The opaque contract lives in prepare: every silent case (unknown email,
// locked, inside the cooldown, ineligible) returns (false, nil), and the
// endpoint's response is identical either way.
func mintAndSendCode(ctx context.Context, d Deps, email, subject, bodyTmpl string,
	prepare func(q *sqlcgen.Queries, codeHash, salt []byte) (bool, error)) error {
	code, err := verifycode.GenerateCode()
	if err != nil {
		return fmt.Errorf("mint code: %w", err)
	}
	salt, err := verifycode.NewSalt()
	if err != nil {
		return fmt.Errorf("mint salt: %w", err)
	}
	send := false
	err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
		var perr error
		send, perr = prepare(sqlcgen.New(tx), verifycode.Hash(salt, code), salt)
		return perr
	})
	if err != nil {
		return err
	}
	if send && d.Mailer != nil {
		_ = d.Mailer.Send(ctx, mailer.Message{
			From: d.MailFrom, To: []string{email},
			Subject: subject,
			Body:    fmt.Sprintf(bodyTmpl, code),
		})
	}
	return nil
}

func resetRequest(d Deps) func(context.Context, *resetRequestInput) (*struct{}, error) {
	return func(ctx context.Context, in *resetRequestInput) (*struct{}, error) {
		projectID, _, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}
		// Invalid addresses get the same opaque 202 as unknown ones.
		if !validateEmail(in.Body.Email) {
			return nil, nil
		}
		now := apiNow(d)
		email := in.Body.Email

		err = mintAndSendCode(ctx, d, email, mailerResetSubject, mailerResetBodyTmpl,
			func(q *sqlcgen.Queries, codeHash, salt []byte) (bool, error) {
				row, qerr := q.GetPlayerPasswordResetState(ctx, sqlcgen.GetPlayerPasswordResetStateParams{
					ProjectID: projectID, Email: &email,
				})
				if errors.Is(qerr, pgx.ErrNoRows) {
					return false, nil
				}
				if qerr != nil {
					return false, qerr
				}
				// No credentials, locked, and inside-cooldown all stay silent.
				if len(row.PasswordHash) == 0 {
					return false, nil
				}
				if row.PasswordResetLockedUntil.Valid && verifycode.AccountLocked(row.PasswordResetLockedUntil.Time, now) {
					return false, nil
				}
				if row.PasswordResetLastSentAt.Valid && !verifycode.CanResend(row.PasswordResetLastSentAt.Time, now) {
					return false, nil
				}
				if qerr := q.SetPlayerPasswordResetChallenge(ctx, sqlcgen.SetPlayerPasswordResetChallengeParams{
					CodeHash:  codeHash,
					Salt:      salt,
					ExpiresAt: pgtype.Timestamptz{Time: now.Add(verifycode.CodeTTL), Valid: true},
					ID:        row.ID,
				}); qerr != nil {
					return false, qerr
				}
				return true, nil
			})
		if err != nil {
			return nil, serverError(ctx, "password reset: request", err)
		}
		return nil, nil
	}
}

func resetConfirm(d Deps) func(context.Context, *resetConfirmInput) (*struct{}, error) {
	return func(ctx context.Context, in *resetConfirmInput) (*struct{}, error) {
		projectID, _, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}
		// Validate the replacement before touching the challenge, so a typo'd
		// password never consumes the code.
		if !validPassword(in.Body.NewPassword) {
			return nil, huma.Error400BadRequest("new_password must be 8–72 characters")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(in.Body.NewPassword), bcryptCost)
		if err != nil {
			return nil, serverError(ctx, "password reset: hash", err)
		}
		now := apiNow(d)
		email := in.Body.Email

		var badCode, lockedAfterAttempt bool
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			row, qerr := q.GetPlayerPasswordResetState(ctx, sqlcgen.GetPlayerPasswordResetStateParams{
				ProjectID: projectID, Email: &email,
			})
			if qerr != nil {
				return qerr
			}
			var cerr error
			lockedAfterAttempt, badCode, cerr = checkCodeChallenge(ctx, challengeState{
				ID:          row.ID,
				CodeHash:    row.PasswordResetCodeHash,
				Salt:        row.PasswordResetSalt,
				ExpiresAt:   row.PasswordResetExpiresAt,
				LockedUntil: row.PasswordResetLockedUntil,
			}, in.Body.Code, now,
				func(ctx context.Context, id int64, max int32) (int32, error) {
					r, rerr := q.ReservePlayerPasswordResetAttempt(ctx, sqlcgen.ReservePlayerPasswordResetAttemptParams{
						ID: id, MaxAttempts: max,
					})
					if rerr != nil {
						return 0, rerr
					}
					return r.PasswordResetLifetimeAttempts, nil
				},
				func(ctx context.Context, id int64, until pgtype.Timestamptz) error {
					return q.LockPlayerPasswordReset(ctx, sqlcgen.LockPlayerPasswordResetParams{
						ID: id, LockedUntil: until,
					})
				},
				q.ClearPlayerPasswordResetLock)
			if cerr != nil {
				return cerr
			}
			if lockedAfterAttempt || badCode {
				return nil
			}
			rows, qerr := q.CompletePlayerPasswordReset(ctx, sqlcgen.CompletePlayerPasswordResetParams{
				PasswordHash: hash, ID: row.ID, CodeHash: row.PasswordResetCodeHash,
			})
			if qerr != nil {
				return qerr
			}
			if rows == 0 {
				// A concurrent re-request replaced the challenge after the
				// compare; the code the caller used is no longer the live one.
				// Commit so the reserved attempt survives.
				badCode = true
				return nil
			}
			if _, qerr := q.RevokeActivePlayerSessions(ctx, sqlcgen.RevokeActivePlayerSessionsParams{
				ProjectID: projectID, PlayerID: row.ID,
			}); qerr != nil {
				return qerr
			}
			return auditlog.Write(ctx, tx, row.ID, "auth.password_reset", "", nil)
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows), errors.Is(err, errVerifyExpired):
			d.Metrics.Verification(observability.VerifyInvalid)
			return nil, huma.Error400BadRequest("invalid email or code")
		case errors.Is(err, errVerifyExhausted), errors.Is(err, errVerifyAccountLocked):
			d.Metrics.Verification(observability.VerifyThrottled)
			return nil, huma.Error429TooManyRequests("too many attempts")
		case err != nil:
			return nil, serverError(ctx, "password reset: confirm tx", err)
		}
		if lockedAfterAttempt {
			d.Metrics.Verification(observability.VerifyThrottled)
			return nil, huma.Error429TooManyRequests("too many attempts")
		}
		if badCode {
			d.Metrics.Verification(observability.VerifyInvalid)
			return nil, huma.Error400BadRequest("invalid email or code")
		}
		return nil, nil
	}
}

func resendVerify(d Deps) func(context.Context, *resendVerifyInput) (*struct{}, error) {
	return func(ctx context.Context, in *resendVerifyInput) (*struct{}, error) {
		projectID, _, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}
		if !validateEmail(in.Body.Email) {
			return nil, nil
		}
		now := apiNow(d)
		email := in.Body.Email

		err = mintAndSendCode(ctx, d, email, mailerVerifySubject, mailerVerifyBodyTmpl,
			func(q *sqlcgen.Queries, codeHash, salt []byte) (bool, error) {
				row, qerr := q.GetPlayerVerificationState(ctx, sqlcgen.GetPlayerVerificationStateParams{
					ProjectID: projectID, Email: &email,
				})
				if errors.Is(qerr, pgx.ErrNoRows) {
					return false, nil
				}
				if qerr != nil {
					return false, qerr
				}
				// Verified, locked, and inside-cooldown all stay silent.
				if row.EmailVerifiedAt.Valid {
					return false, nil
				}
				if row.EmailVerificationLockedUntil.Valid && verifycode.AccountLocked(row.EmailVerificationLockedUntil.Time, now) {
					return false, nil
				}
				if row.EmailVerificationLastSentAt.Valid && !verifycode.CanResend(row.EmailVerificationLastSentAt.Time, now) {
					return false, nil
				}
				if qerr := q.SetPlayerVerificationCode(ctx, sqlcgen.SetPlayerVerificationCodeParams{
					ProjectID:                  projectID,
					ID:                         row.ID,
					EmailVerificationCodeHash:  codeHash,
					EmailVerificationSalt:      salt,
					EmailVerificationExpiresAt: pgtype.Timestamptz{Time: now.Add(verifycode.CodeTTL), Valid: true},
				}); qerr != nil {
					return false, qerr
				}
				return true, nil
			})
		if err != nil {
			return nil, serverError(ctx, "verify resend", err)
		}
		return nil, nil
	}
}
