package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	// friendCodeAlphabet matches the join-code alphabet: no I, O, 0, 1, so a
	// code survives being read aloud or handwritten.
	friendCodeAlphabet    = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	friendCodeLen         = 8
	friendCodeMaxAttempts = 5
)

type friendCodeResult struct {
	FriendCode string `json:"friend_code" example:"XKCD4242"`
}

type friendCodeOutput struct {
	Body friendCodeResult
}

type friendCodeResolveInput struct {
	Code string `path:"code" minLength:"1" maxLength:"32" example:"XKCD4242"`
}

func registerFriendCodeRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "regenerateFriendCode",
		Method:      http.MethodPost,
		Path:        "/v1/profile/friend-code",
		Summary:     "Regenerate the caller's friend code",
		Description: "Mints a new code and invalidates the old one immediately. The " +
			"current code is on GET /v1/profile.",
		Tags:     []string{"Friends & Presence"},
		Security: playerSecurity,
	}, friendCodeRegenerate(d))

	huma.Register(api, huma.Operation{
		OperationID: "resolveFriendCode",
		Method:      http.MethodGet,
		Path:        "/v1/players/by-code/{code}",
		Summary:     "Resolve a friend code to a public player",
		Description: "Case-insensitive; dashes and spaces are ignored. Returns public " +
			"fields only — use the resolved id with POST /v1/friends/{player_id}/request. " +
			"Codes are 8 characters from a 32-letter alphabet and the endpoint sits " +
			"behind the per-player rate limit, so code scanning is impractical. " +
			"Unknown and cross-project codes are 404.",
		Tags:     []string{"Friends & Presence"},
		Security: playerSecurity,
	}, friendCodeResolve(d))
}

func newFriendCode() (string, error) {
	return webutil.RandomCode(friendCodeAlphabet, friendCodeLen)
}

// normalizeFriendCode uppercases and strips the separators people add when a
// code travels by voice or chat.
func normalizeFriendCode(raw string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(raw))
}

// ensureFriendCode lazily initializes the caller's code on first profile
// read: every player has a code with no setup step, and no backfill was
// needed. Retries a (vanishingly rare) code collision; a concurrent
// initializer winning the race is read back, not an error.
func ensureFriendCode(ctx context.Context, d Deps, me int64) (string, error) {
	for attempt := 0; ; attempt++ {
		code, err := newFriendCode()
		if err != nil {
			return "", err
		}
		var rows int64
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			var qerr error
			rows, qerr = sqlcgen.New(tx).SetPlayerFriendCodeIfAbsent(ctx, sqlcgen.SetPlayerFriendCodeIfAbsentParams{
				FriendCode: &code, ID: me,
			})
			return qerr
		})
		switch {
		case webutil.IsUniqueViolation(err) && attempt < friendCodeMaxAttempts:
			continue
		case err != nil:
			return "", err
		case rows > 0:
			return code, nil
		}
		// Read the concurrent initializer's code back from the primary — a
		// lagging replica may not see it yet.
		var existing *string
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			row, qerr := sqlcgen.New(tx).GetProfile(ctx, me)
			if qerr != nil {
				return qerr
			}
			existing = row.FriendCode
			return nil
		})
		if err != nil {
			return "", err
		}
		if existing == nil {
			return "", errors.New("friend code: unset after initialization")
		}
		return *existing, nil
	}
}

func friendCodeRegenerate(d Deps) func(context.Context, *struct{}) (*friendCodeOutput, error) {
	return func(ctx context.Context, _ *struct{}) (*friendCodeOutput, error) {
		me, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		for attempt := 0; ; attempt++ {
			code, err := newFriendCode()
			if err != nil {
				return nil, serverError(ctx, "friend code: rand", err)
			}
			var rows int64
			err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
				var qerr error
				rows, qerr = sqlcgen.New(tx).SetPlayerFriendCode(ctx, sqlcgen.SetPlayerFriendCodeParams{
					FriendCode: &code, ID: me,
				})
				return qerr
			})
			switch {
			case webutil.IsUniqueViolation(err) && attempt < friendCodeMaxAttempts:
				continue
			case err != nil:
				return nil, serverError(ctx, "friend code: regenerate", err)
			case rows == 0:
				return nil, huma.Error404NotFound("not found")
			}
			return &friendCodeOutput{Body: friendCodeResult{FriendCode: code}}, nil
		}
	}
}

func friendCodeResolve(d Deps) func(context.Context, *friendCodeResolveInput) (*playerGetOutput, error) {
	return func(ctx context.Context, in *friendCodeResolveInput) (*playerGetOutput, error) {
		if _, ok := playerauth.IDFromContext(ctx); !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			return nil, huma.Error400BadRequest("api key has no project pin")
		}

		code := normalizeFriendCode(in.Code)
		var resp publicPlayerResponse
		err := d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
			row, qerr := sqlcgen.New(tx).GetPublicPlayerByFriendCode(ctx, sqlcgen.GetPublicPlayerByFriendCodeParams{
				FriendCode: &code, ProjectID: projectID,
			})
			if qerr != nil {
				return qerr
			}
			resp = publicPlayer(row.ID, row.DisplayName, row.CreatedAt)
			return nil
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("player not found")
		}
		if err != nil {
			return nil, serverError(ctx, "friend code: resolve", err)
		}
		return &playerGetOutput{Body: resp}, nil
	}
}
