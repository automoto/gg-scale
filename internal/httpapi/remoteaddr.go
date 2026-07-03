package httpapi

import (
	"errors"
	"net/http"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/webutil"
)

// remoteAddrMaxLen caps a remote-address string. These are opaque endpoint
// strings (IP, Tailscale name, iroh EndpointID, …) — we never parse them.
const remoteAddrMaxLen = 255

type remoteAddrPayload struct {
	Primary   *string `json:"primary_remote_addr"`
	Secondary *string `json:"secondary_remote_addr"`
}

// validRemoteAddr enforces length + printable-non-control only (no format
// guessing), per CLAUDE.md's stdlib-helper preference. Empty is allowed (it
// clears the field).
func validRemoteAddr(s string) bool {
	if len(s) > remoteAddrMaxLen {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func mountRemoteAddrRoutes(r chi.Router, d Deps) {
	r.Get("/account/remote-addrs", ownerRemoteAddrGetHandler(d))
	r.Put("/account/remote-addrs", ownerRemoteAddrPutHandler(d))
	r.Get("/friends/{player_id}/remote-addrs", friendRemoteAddrGetHandler(d))
}

// resolveCallerAccount returns the caller player's linked account, or false
// (with a 403 already written) when the player is anonymous / unlinked.
func resolveCallerAccount(w http.ResponseWriter, r *http.Request, d Deps) (pgtype.UUID, bool) {
	playerID, ok := playerauth.IDFromContext(r.Context())
	if !ok {
		http.Error(w, "no player", http.StatusUnauthorized)
		return pgtype.UUID{}, false
	}
	var acc pgtype.UUID
	err := d.Pool.Q(r.Context(), func(tx pgx.Tx) error {
		var e error
		acc, e = sqlcgen.New(tx).GetPlayerLinkedAccountID(r.Context(), playerID)
		return e
	})
	if err != nil || !acc.Valid {
		http.Error(w, linkAccountMsg, http.StatusForbidden)
		return pgtype.UUID{}, false
	}
	return acc, true
}

func ownerRemoteAddrGetHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acc, ok := resolveCallerAccount(w, r, d)
		if !ok {
			return
		}
		writeRemoteAddrs(w, d, r, acc)
	}
}

func ownerRemoteAddrPutHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		acc, ok := resolveCallerAccount(w, r, d)
		if !ok {
			return
		}
		var req remoteAddrPayload
		if !decodeJSON(w, r, &req) {
			return
		}
		if (req.Primary != nil && !validRemoteAddr(*req.Primary)) ||
			(req.Secondary != nil && !validRemoteAddr(*req.Secondary)) {
			http.Error(w, "remote address too long or contains control characters", http.StatusBadRequest)
			return
		}
		err := d.Pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
			return sqlcgen.New(tx).SetPlayerAccountRemoteAddrs(r.Context(), sqlcgen.SetPlayerAccountRemoteAddrsParams{
				ID:                  acc,
				PrimaryRemoteAddr:   normalizeAddr(req.Primary),
				SecondaryRemoteAddr: normalizeAddr(req.Secondary),
			})
		})
		if err != nil {
			webutil.InternalError(w, "remote-addr put", err)
			return
		}
		writeRemoteAddrs(w, d, r, acc)
	}
}

// friendRemoteAddrGetHandler lets an ACCEPTED friend read the target's remote
// addresses. Non-friends, blocked players, and unlinked callers are denied.
func friendRemoteAddrGetHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, ok := resolveCallerAccount(w, r, d)
		if !ok {
			return
		}
		targetPlayer, ok := pathInt64(r, "player_id")
		if !ok {
			http.Error(w, "player_id required", http.StatusBadRequest)
			return
		}
		var targetAcc pgtype.UUID
		err := d.Pool.Q(r.Context(), func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			acc, e := q.GetPlayerLinkedAccountID(r.Context(), targetPlayer)
			if e != nil {
				return e
			}
			if !acc.Valid {
				return errTargetNoAccount
			}
			targetAcc = acc
			// Must be accepted friends AND not blocked in either direction.
			if _, be := q.IsBlockedBetweenAccounts(r.Context(), sqlcgen.IsBlockedBetweenAccountsParams{A: me, B: targetAcc}); be == nil {
				return errFriendBlocked
			} else if !errors.Is(be, pgx.ErrNoRows) {
				return be
			}
			if _, fe := q.AreAccountsFriendsAccepted(r.Context(), sqlcgen.AreAccountsFriendsAcceptedParams{A: me, B: targetAcc}); fe != nil {
				if errors.Is(fe, pgx.ErrNoRows) {
					return errNotFriends
				}
				return fe
			}
			return nil
		})
		switch {
		case errors.Is(err, errTargetNoAccount), errors.Is(err, pgx.ErrNoRows):
			http.Error(w, "not found", http.StatusNotFound)
			return
		case errors.Is(err, errNotFriends), errors.Is(err, errFriendBlocked):
			// Don't distinguish non-friend from blocked.
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		case err != nil:
			webutil.InternalError(w, "friend remote-addr", err)
			return
		}
		writeRemoteAddrs(w, d, r, targetAcc)
	}
}

// serverRemoteAddrGetHandler is the secret-key server-tier read: a game server
// reads a player's remote addresses for a player linked to the key's project.
// Mounted only under RequireKeyType(secret), so publishable keys never reach it.
func serverRemoteAddrGetHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID, ok := db.ProjectFromContext(r.Context())
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}
		targetPlayer, ok := pathInt64(r, "player_id")
		if !ok {
			http.Error(w, "player_id required", http.StatusBadRequest)
			return
		}
		var acc pgtype.UUID
		err := d.Pool.Q(r.Context(), func(tx pgx.Tx) error {
			a, e := sqlcgen.New(tx).GetPlayerAccountForProjectRead(r.Context(), sqlcgen.GetPlayerAccountForProjectReadParams{
				ID: targetPlayer, ProjectID: projectID,
			})
			if e != nil {
				return e
			}
			if !a.Valid {
				return errTargetNoAccount
			}
			acc = a
			return nil
		})
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errTargetNoAccount) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			webutil.InternalError(w, "server remote-addr", err)
			return
		}
		writeRemoteAddrs(w, d, r, acc)
	}
}

func writeRemoteAddrs(w http.ResponseWriter, d Deps, r *http.Request, acc pgtype.UUID) {
	var row sqlcgen.GetPlayerAccountRemoteAddrsRow
	if err := d.Pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var e error
		row, e = sqlcgen.New(tx).GetPlayerAccountRemoteAddrs(r.Context(), acc)
		return e
	}); err != nil {
		webutil.InternalError(w, "remote-addr read", err)
		return
	}
	writeJSON(w, remoteAddrPayload{
		Primary:   row.PrimaryRemoteAddr,
		Secondary: row.SecondaryRemoteAddr,
	})
}

func normalizeAddr(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}
