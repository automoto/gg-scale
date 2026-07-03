package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/verifycode"
	"github.com/ggscale/ggscale/internal/webutil"
)

const xuidMaxChars = 64

// validateXUID accepts a 1–64 char printable secondary identifier. Control
// characters are rejected so the value is safe to surface in rosters/logs.
func validateXUID(s string) bool {
	if n := utf8.RuneCountInString(s); n == 0 || n > xuidMaxChars {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

type profileResponse struct {
	ID              int64  `json:"id"`
	ProjectID       int64  `json:"project_id"`
	ExternalID      string `json:"external_id"`
	Email           string `json:"email,omitempty"`
	XUID            string `json:"xuid,omitempty"`
	EmailVerifiedAt string `json:"email_verified_at,omitempty"`
	CreatedAt       string `json:"created_at"`
}

type profilePatchRequest struct {
	Email *string `json:"email,omitempty"`
	XUID  *string `json:"xuid,omitempty"`
}

// GET /v1/profile
func profileGetHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		me, ok := playerauth.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no player", http.StatusUnauthorized)
			return
		}

		var resp profileResponse
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			row, qerr := sqlcgen.New(tx).GetProfile(ctx, me)
			if qerr != nil {
				return qerr
			}
			resp = profileResponse{
				ID: row.ID, ProjectID: row.ProjectID, ExternalID: row.ExternalID,
				CreatedAt: row.CreatedAt.Time.UTC().Format(time.RFC3339),
			}
			if row.Email != nil {
				resp.Email = *row.Email
			}
			if row.Xuid != nil {
				resp.XUID = *row.Xuid
			}
			if row.EmailVerifiedAt.Valid {
				resp.EmailVerifiedAt = row.EmailVerifiedAt.Time.UTC().Format(time.RFC3339)
			}
			return nil
		})
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			webutil.InternalError(w, "profile get: tx", err)
			return
		}
		writeJSON(w, resp)
	}
}

// PATCH /v1/profile. Editable fields: email and xuid. A new
// email triggers a verification round-trip (clears email_verified_at, mints a
// new verification token, sends mail) and returns 202; an xuid-only change
// returns 204.
func profilePatchHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req profilePatchRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Email == nil && req.XUID == nil {
			http.Error(w, "no editable fields supplied", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		me, ok := playerauth.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no player", http.StatusUnauthorized)
			return
		}

		if req.XUID != nil {
			if status, err := updateXUID(ctx, d, me, *req.XUID); err != nil {
				http.Error(w, err.Error(), status)
				return
			}
			if req.Email == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}

		newEmail := *req.Email
		if !validateEmail(newEmail) {
			http.Error(w, "email invalid", http.StatusBadRequest)
			return
		}

		code, err := verifycode.GenerateCode()
		if err != nil {
			webutil.InternalError(w, "profile patch: code", err)
			return
		}
		salt, err := verifycode.NewSalt()
		if err != nil {
			webutil.InternalError(w, "profile patch: salt", err)
			return
		}
		codeHash := verifycode.Hash(salt, code)

		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			return sqlcgen.New(tx).UpdateProfileEmail(ctx, sqlcgen.UpdateProfileEmailParams{
				ID:                         me,
				Email:                      &newEmail,
				EmailVerificationCodeHash:  codeHash,
				EmailVerificationSalt:      salt,
				EmailVerificationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(verifycode.CodeTTL), Valid: true},
			})
		})
		if err != nil {
			webutil.InternalError(w, "profile patch: tx", err)
			return
		}

		if d.Mailer != nil {
			_ = d.Mailer.Send(ctx, mailer.Message{
				From: d.MailFrom, To: []string{newEmail},
				Subject: mailerVerifySubject,
				Body:    fmt.Sprintf("Your ggscale verification code is %s (valid 15 minutes).", code),
			})
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

// updateXUID sets (or, for an empty string, clears) the caller's xuid. It
// returns an HTTP status + error on a validation or uniqueness failure.
func updateXUID(ctx context.Context, d Deps, me int64, raw string) (int, error) {
	var xuid *string
	if raw != "" {
		if !validateXUID(raw) {
			return http.StatusBadRequest, errors.New("xuid invalid (1–64 printable chars)")
		}
		xuid = &raw
	}
	err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).UpdateProfileXuid(ctx, sqlcgen.UpdateProfileXuidParams{ID: me, Xuid: xuid})
	})
	switch {
	case isUniqueViolation(err):
		return http.StatusConflict, errors.New("xuid already in use")
	case err != nil:
		return http.StatusInternalServerError, errors.New("profile patch: xuid")
	}
	return 0, nil
}
