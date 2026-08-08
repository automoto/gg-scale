package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/auditlog"
	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

// maxVerifyBodyBytes caps the verify request body. The body is a single
// JWT (~500–1000 bytes); 8 KiB leaves slack for header verbosity without
// inviting abuse.
const maxVerifyBodyBytes = 8 << 10

type playerVerifyRequest struct {
	SessionToken string `json:"session_token" example:"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiI0MiJ9.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"`
}

// errStaleSession marks a token rejected by the epoch/ban gate. Collapses to
// the same opaque 401 as every other verify failure.
var errStaleSession = errors.New("project_players verify: stale session")

// playerVerifyResponse is what game-servers get back on a valid token.
// ExternalID is the per-game stable identifier (Steam ID, anonymous
// UUID, etc.) — the same column that auth/anonymous returns. Email is
// omitempty because it's optional on the account.
type playerVerifyResponse struct {
	PlayerID   int64  `json:"player_id" example:"42"`
	ExternalID string `json:"external_id" example:"anon_9f86d081884c7d659a2feaa0c55ad015"`
	Email      string `json:"email,omitempty" example:"player@example.com"`
}

// playerSessionVerifyHandler validates a player session token on behalf
// of a game-server. The server-tier API key on the request
// authenticates the CALLER (the game-server workload); the body's
// session_token is the PLAYER's session being verified.
//
// Every failure mode collapses to a single opaque 401 — body decode
// errors included — so a hostile caller can't use the endpoint as a
// probe to distinguish "valid session, user gone" from "tampered token"
// or "malformed body". The per-API-key rate limiter on the outer group
// bounds how many probes a stolen key can attempt; the
// RequireKeyType(KeyTypeSecret) gate on the route keeps publishable
// keys (embedded in shipped game binaries) off this oracle entirely.
//
// See docs/temp/gameserver-auth.md for the design rationale.
//
// This is registered as a huma operation for the spec, but its handler uses a
// body-callback (verifyOutput.Body func) so it takes full manual control of the
// request body and response: huma never parses the body (which would surface a
// distinguishable 400/413/422) — every failure, malformed body included,
// collapses to the same opaque 401.
type playerSessionVerifyOutput struct {
	Body func(huma.Context)
}

func registerPlayerSessionVerify(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "verifyPlayerSession",
		Method:      http.MethodPost,
		Path:        "/v1/server/player-sessions/verify",
		Summary:     "Server-tier: verify a player session token",
		Tags:        []string{"Session Verification"},
		Security:    apiKeySecurity,
	}, func(_ context.Context, _ *struct{}) (*playerSessionVerifyOutput, error) {
		return &playerSessionVerifyOutput{Body: func(hctx huma.Context) {
			r, w := humachi.Unwrap(hctx)
			verifyPlayerSession(d, w, r)
		}}, nil
	})
}

func verifyPlayerSession(d Deps, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	callerTenant, err := db.TenantFromContext(ctx)
	if err != nil {
		writeInvalidSession(w)
		return
	}
	callerProject, ok := db.ProjectFromContext(ctx)
	if !ok {
		writeInvalidSession(w)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxVerifyBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req playerVerifyRequest
	if err := dec.Decode(&req); err != nil {
		writeInvalidSession(w)
		return
	}

	claims, err := d.Signer.Verify(req.SessionToken)
	if err != nil {
		writeInvalidSession(w)
		return
	}
	// Tenant + project pinning: both claims must be non-zero AND
	// match the caller's API-key bindings. The non-zero guard
	// closes a latent bypass — a future token issued without a
	// project pin (ProjectID=0) would otherwise short-circuit the
	// project check.
	if claims.TenantID == 0 || claims.TenantID != callerTenant {
		writeInvalidSession(w)
		return
	}
	if claims.ProjectID == 0 || claims.ProjectID != callerProject {
		writeInvalidSession(w)
		return
	}

	var row sqlcgen.GetPlayerForVerifyRow
	err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var qerr error
		row, qerr = q.GetPlayerForVerify(ctx, claims.PlayerID)
		if qerr != nil {
			return qerr
		}
		// Reject a session whose epoch is stale — the player was disabled
		// or tenant-banned after this token was minted (both bump
		// session_epoch). Makes revocation immediate, not TTL-bounded.
		if claims.SessionEpoch != int64(row.SessionEpoch) {
			return errStaleSession
		}
		// Tenant-ban gate: even inside the epoch window, a banned linked
		// account cannot be verified.
		if row.PlayerAccountID.Valid {
			if _, berr := q.IsAccountBannedInTenant(ctx, sqlcgen.IsAccountBannedInTenantParams{
				TenantID: row.TenantID, PlayerAccountID: row.PlayerAccountID,
			}); berr == nil {
				return errStaleSession
			} else if !errors.Is(berr, pgx.ErrNoRows) {
				return berr
			}
		}
		return auditlog.Write(ctx, tx, row.ID, "auth.server_verify", "", nil)
	})
	if errors.Is(err, errStaleSession) {
		writeInvalidSession(w)
		return
	}
	if err != nil {
		// ErrNoRows is the expected miss (deleted/disabled user,
		// soft-deleted project/tenant, or wrong tenant under RLS).
		// Any other error is a real DB problem; log server-side
		// but collapse to the same opaque 401 on the wire.
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.ErrorContext(ctx, "project_players verify: lookup", "err", err)
		}
		writeInvalidSession(w)
		return
	}

	// Defense in depth: the SQL query already enforces tenant via
	// the explicit predicate and soft-delete via the JOINs, but if
	// either is ever removed the row check here catches drift.
	if row.TenantID != callerTenant || row.ProjectID != callerProject {
		writeInvalidSession(w)
		return
	}

	writeJSON(w, playerVerifyResponse{
		PlayerID:   row.ID,
		ExternalID: row.ExternalID,
		Email:      row.Email,
	})
}

// writeInvalidSession returns the opaque 401 used by every failure
// mode of the verify endpoint. Centralised so the wire shape (status,
// content-type, body) is impossible to drift between sites.
func writeInvalidSession(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"invalid session"}`))
}
