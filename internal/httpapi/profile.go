package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
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

const (
	xuidMaxChars        = 64
	displayNameMaxChars = 64
)

// validPrintableName accepts 1–max printable characters. Control characters
// are rejected so the value is safe to surface in rosters/logs.
func validPrintableName(s string, max int) bool {
	if n := utf8.RuneCountInString(s); n == 0 || n > max {
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
	DisplayName     string `json:"display_name,omitempty" example:"Nova Fox"`
	FriendCode      string `json:"friend_code,omitempty" example:"XKCD4242"`
	EmailVerifiedAt string `json:"email_verified_at,omitempty" example:"2026-01-02T15:04:05Z"`
	CreatedAt       string `json:"created_at" example:"2026-01-02T15:04:05Z"`
}

type profilePatchRequest struct {
	Email       *string `json:"email,omitempty" example:"player@example.com"`
	XUID        *string `json:"xuid,omitempty" example:"2533274790395904"`
	DisplayName *string `json:"display_name,omitempty" example:"Nova Fox"`
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
		OperationID: "patchProfile",
		Method:      http.MethodPatch,
		Path:        "/v1/profile",
		Summary:     "Update the caller's email, xuid, or display name",
		Description: "Setting display_name requires a linked gg-scale account; " +
			"an empty string clears it.",
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
			if row.DisplayName != nil {
				resp.DisplayName = *row.DisplayName
			}
			if row.FriendCode != nil {
				resp.FriendCode = *row.FriendCode
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
		if resp.FriendCode == "" {
			code, cerr := ensureFriendCode(ctx, d, me)
			if cerr != nil {
				return nil, serverError(ctx, "profile get: friend code", cerr)
			}
			resp.FriendCode = code
		}
		return &profileGetOutput{Body: resp}, nil
	}
}

// errNoLinkedAccount rejects a display-name change for an anonymous /
// unlinked player: there is no global account to carry the name.
var errNoLinkedAccount = errors.New("profile: no linked account")

// normalizedDisplayName trims and validates a display_name patch value. Both
// an absent field and an empty (post-trim) string return nil — the caller
// distinguishes "skip" from "clear" by whether the field was present.
func normalizedDisplayName(raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	name := strings.TrimSpace(*raw)
	if name == "" {
		return nil, nil
	}
	if !validPrintableName(name, displayNameMaxChars) {
		return nil, huma.Error400BadRequest("display_name invalid (1–64 printable chars)")
	}
	return &name, nil
}

// normalizedXUID validates a xuid patch value; an empty string means clear
// and returns nil.
func normalizedXUID(raw *string) (*string, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}
	if !validPrintableName(*raw, xuidMaxChars) {
		return nil, huma.Error400BadRequest("xuid invalid (1–64 printable chars)")
	}
	return raw, nil
}

// profilePatch edits email, xuid, and/or display_name. Every field is
// validated before anything is written, and all writes share one transaction,
// so a rejected field never leaves another field committed. A new email
// triggers a verification round-trip (clears email_verified_at, mints a new
// verification token, sends mail) and returns 202; changes without an email
// return 204.
func profilePatch(d Deps) func(context.Context, *profilePatchInput) (*profilePatchOutput, error) {
	return func(ctx context.Context, in *profilePatchInput) (*profilePatchOutput, error) {
		req := in.Body
		if req.Email == nil && req.XUID == nil && req.DisplayName == nil {
			return nil, huma.Error400BadRequest("no editable fields supplied")
		}

		me, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}

		namePtr, err := normalizedDisplayName(req.DisplayName)
		if err != nil {
			return nil, err
		}
		xuidPtr, err := normalizedXUID(req.XUID)
		if err != nil {
			return nil, err
		}
		if req.Email != nil && !validateEmail(*req.Email) {
			return nil, huma.Error400BadRequest("email invalid")
		}
		var code string
		var codeHash, salt []byte
		if req.Email != nil {
			if code, err = verifycode.GenerateCode(); err != nil {
				return nil, serverError(ctx, "profile patch: code", err)
			}
			if salt, err = verifycode.NewSalt(); err != nil {
				return nil, serverError(ctx, "profile patch: salt", err)
			}
			codeHash = verifycode.Hash(salt, code)
		}

		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			if req.DisplayName != nil {
				acc, qerr := q.GetPlayerLinkedAccountID(ctx, me)
				if qerr != nil && !errors.Is(qerr, pgx.ErrNoRows) {
					return qerr
				}
				switch {
				case !acc.Valid && namePtr == nil:
					// An unlinked player clearing a name it cannot have is a
					// quiet no-op, not a 403.
				case !acc.Valid:
					return errNoLinkedAccount
				default:
					// player_accounts is global (plain grants, no RLS), so
					// the write joins this tenant-scoped transaction directly.
					if qerr := q.SetPlayerAccountDisplayName(ctx, sqlcgen.SetPlayerAccountDisplayNameParams{
						ID: acc, DisplayName: namePtr,
					}); qerr != nil {
						return qerr
					}
				}
			}
			if req.XUID != nil {
				if qerr := q.UpdateProfileXuid(ctx, sqlcgen.UpdateProfileXuidParams{ID: me, Xuid: xuidPtr}); qerr != nil {
					return qerr
				}
			}
			if req.Email != nil {
				return q.UpdateProfileEmail(ctx, sqlcgen.UpdateProfileEmailParams{
					ID:                         me,
					Email:                      req.Email,
					EmailVerificationCodeHash:  codeHash,
					EmailVerificationSalt:      salt,
					EmailVerificationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(verifycode.CodeTTL), Valid: true},
				})
			}
			return nil
		})
		switch {
		case errors.Is(err, errNoLinkedAccount):
			return nil, huma.Error403Forbidden("link a gg-scale account to set a display name")
		case webutil.UniqueViolationConstraint(err) == "project_players_email_uniq":
			return nil, huma.Error409Conflict("email already in use")
		case webutil.IsUniqueViolation(err):
			return nil, huma.Error409Conflict("xuid already in use")
		case err != nil:
			return nil, serverError(ctx, "profile patch: tx", err)
		}

		if req.Email == nil {
			return &profilePatchOutput{Status: http.StatusNoContent}, nil
		}
		if d.Mailer != nil {
			_ = d.Mailer.Send(ctx, mailer.Message{
				From: d.MailFrom, To: []string{*req.Email},
				Subject: mailerVerifySubject,
				Body:    fmt.Sprintf("Your ggscale verification code is %s (valid 15 minutes).", code),
			})
		}
		return &profilePatchOutput{Status: http.StatusAccepted}, nil
	}
}
