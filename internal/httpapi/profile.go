package httpapi

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/enduser"
	"github.com/ggscale/ggscale/internal/mailer"
)

type profileResponse struct {
	ID              int64  `json:"id"`
	ProjectID       int64  `json:"project_id"`
	ExternalID      string `json:"external_id"`
	Email           string `json:"email,omitempty"`
	EmailVerifiedAt string `json:"email_verified_at,omitempty"`
	CreatedAt       string `json:"created_at"`
}

type profilePatchRequest struct {
	Email *string `json:"email,omitempty"`
}

// GET /v1/profile — m1.md 4.5.1.
func profileGetHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		me, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
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
			internalError(w, "profile get: tx", err)
			return
		}
		writeJSON(w, resp)
	}
}

// PATCH /v1/profile — m1.md 4.5.2. Only the email field is editable in
// Phase 1; new email triggers a verification round-trip (clears
// email_verified_at, mints a new verification token, sends mail).
func profilePatchHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req profilePatchRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Email == nil {
			http.Error(w, "no editable fields supplied", http.StatusBadRequest)
			return
		}
		newEmail := *req.Email
		if !validateEmail(newEmail) {
			http.Error(w, "email invalid", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		me, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
		}

		verifyToken, err := randomHex("", 32)
		if err != nil {
			internalError(w, "profile patch: rand", err)
			return
		}
		verifyHash := sha256.Sum256([]byte(verifyToken))

		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			return sqlcgen.New(tx).UpdateProfileEmail(ctx, sqlcgen.UpdateProfileEmailParams{
				ID:                         me,
				Email:                      &newEmail,
				EmailVerificationHash:      verifyHash[:],
				EmailVerificationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(verifyTokenTTL), Valid: true},
			})
		})
		if err != nil {
			internalError(w, "profile patch: tx", err)
			return
		}

		if d.Mailer != nil {
			_ = d.Mailer.Send(ctx, mailer.Message{
				From: d.MailFrom, To: []string{newEmail},
				Subject: mailerVerifySubject,
				Body:    "Verification token: " + verifyToken + " (valid 24h)",
			})
		}
		w.WriteHeader(http.StatusAccepted)
	}
}
