package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/auditlog"
	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

// maxVerifyBodyBytes caps the verify request body. The body is a single
// JWT (~500–1000 bytes); 8 KiB leaves slack for header verbosity without
// inviting abuse.
const maxVerifyBodyBytes = 8 << 10

type playerVerifyRequest struct {
	SessionToken string `json:"session_token"`
}

// errStaleSession marks a token rejected by the epoch/ban gate. Collapses to
// the same opaque 401 as every other verify failure.
var errStaleSession = errors.New("project_players verify: stale session")

// playerVerifyResponse is what game-servers get back on a valid token.
// ExternalID is the per-game stable identifier (Steam ID, anonymous
// UUID, etc.) — the same column that auth/anonymous returns. Email is
// omitempty because it's optional on the account.
type playerVerifyResponse struct {
	PlayerID   int64  `json:"player_id"`
	ExternalID string `json:"external_id"`
	Email      string `json:"email,omitempty"`
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
func playerSessionVerifyHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
}

// writeInvalidSession returns the opaque 401 used by every failure
// mode of the verify endpoint. Centralised so the wire shape (status,
// content-type, body) is impossible to drift between sites.
func writeInvalidSession(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"invalid session"}`))
}
