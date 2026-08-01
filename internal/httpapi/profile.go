package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
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
	ID              int64  `json:"id" example:"42"`
	ProjectID       int64  `json:"project_id" example:"7"`
	ExternalID      string `json:"external_id" example:"user_1b4e28ba2fa14f0e8bf1a09b4d7e5f60"`
	Email           string `json:"email,omitempty" example:"player@example.com"`
	XUID            string `json:"xuid,omitempty" example:"2533274790395904"`
	EmailVerifiedAt string `json:"email_verified_at,omitempty" example:"2026-01-02T15:04:05Z"`
	CreatedAt       string `json:"created_at" example:"2026-01-02T15:04:05Z"`
}

type profilePatchRequest struct {
	Email *string `json:"email,omitempty" example:"player@example.com"`
	XUID  *string `json:"xuid,omitempty" example:"2533274790395904"`
}

type profileGetOutput struct {
	Body profileResponse
}

type profilePatchInput struct {
	Body profilePatchRequest
}

// profilePatchOutput carries no body; huma reads the Status field to pick 202
// (email change → verification round-trip) vs 204 (xuid-only change).
type profilePatchOutput struct {
	Status int
}

func registerProfileRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "getProfile",
		Method:      http.MethodGet,
		Path:        "/v1/profile",
		Summary:     "Get the caller's profile",
		Tags:        []string{"Player Profiles"},
		Security:    playerSecurity,
	}, profileGet(d))

	huma.Register(api, huma.Operation{
		OperationID:   "patchProfile",
		Method:        http.MethodPatch,
		Path:          "/v1/profile",
		Summary:       "Update the caller's email or xuid",
		Tags:          []string{"Player Profiles"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusAccepted,
	}, profilePatch(d))
}

func profileGet(d Deps) func(context.Context, *struct{}) (*profileGetOutput, error) {
	return func(ctx context.Context, _ *struct{}) (*profileGetOutput, error) {
		me, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
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
			return nil, huma.Error404NotFound("not found")
		}
		if err != nil {
			return nil, serverError(ctx, "profile get: tx", err)
		}
		return &profileGetOutput{Body: resp}, nil
	}
}

// profilePatch edits email and/or xuid. A new email triggers a verification
// round-trip (clears email_verified_at, mints a new verification token, sends
// mail) and returns 202; an xuid-only change returns 204.
func profilePatch(d Deps) func(context.Context, *profilePatchInput) (*profilePatchOutput, error) {
	return func(ctx context.Context, in *profilePatchInput) (*profilePatchOutput, error) {
		req := in.Body
		if req.Email == nil && req.XUID == nil {
			return nil, huma.Error400BadRequest("no editable fields supplied")
		}

		me, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}

		if req.XUID != nil {
			if err := updateXUID(ctx, d, me, *req.XUID); err != nil {
				return nil, err
			}
			if req.Email == nil {
				return &profilePatchOutput{Status: http.StatusNoContent}, nil
			}
		}

		newEmail := *req.Email
		if !validateEmail(newEmail) {
			return nil, huma.Error400BadRequest("email invalid")
		}

		code, err := verifycode.GenerateCode()
		if err != nil {
			return nil, serverError(ctx, "profile patch: code", err)
		}
		salt, err := verifycode.NewSalt()
		if err != nil {
			return nil, serverError(ctx, "profile patch: salt", err)
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
			return nil, serverError(ctx, "profile patch: tx", err)
		}

		if d.Mailer != nil {
			_ = d.Mailer.Send(ctx, mailer.Message{
				From: d.MailFrom, To: []string{newEmail},
				Subject: mailerVerifySubject,
				Body:    fmt.Sprintf("Your ggscale verification code is %s (valid 15 minutes).", code),
			})
		}
		return &profilePatchOutput{Status: http.StatusAccepted}, nil
	}
}

// updateXUID sets (or, for an empty string, clears) the caller's xuid,
// returning a huma error on a validation or uniqueness failure.
func updateXUID(ctx context.Context, d Deps, me int64, raw string) error {
	var xuid *string
	if raw != "" {
		if !validateXUID(raw) {
			return huma.Error400BadRequest("xuid invalid (1–64 printable chars)")
		}
		xuid = &raw
	}
	err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).UpdateProfileXuid(ctx, sqlcgen.UpdateProfileXuidParams{ID: me, Xuid: xuid})
	})
	switch {
	case webutil.IsUniqueViolation(err):
		return huma.Error409Conflict("xuid already in use")
	case err != nil:
		return serverError(ctx, "profile patch: xuid", err)
	}
	return nil
}
