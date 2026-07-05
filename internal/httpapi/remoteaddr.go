package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/remoteaddr"
	"github.com/ggscale/ggscale/internal/webutil"
)

// maxRemoteAddrs is the slot count: LAN IP, public IP, DNS, iroh.
const maxRemoteAddrs = 4

// remoteAddrEntry is one typed address on the wire. Scope is derived
// server-side and ignored on input — declared (not dropped) so a GET body
// round-trips through decodeJSON's DisallowUnknownFields.
type remoteAddrEntry struct {
	Type    string `json:"type"`
	Scope   string `json:"scope,omitempty"`
	Address string `json:"address"`
}

type remoteAddrsPayload struct {
	Addresses []remoteAddrEntry `json:"addresses"`
}

// parseRemoteAddrSet validates a submitted address list into a slot set.
// Errors are user-safe and indexed for per-entry failures.
func parseRemoteAddrSet(entries []remoteAddrEntry) (remoteaddr.Set, error) {
	if len(entries) > maxRemoteAddrs {
		return remoteaddr.Set{}, fmt.Errorf("too many addresses (max %d)", maxRemoteAddrs)
	}
	addrs := make([]remoteaddr.Address, 0, len(entries))
	for i, e := range entries {
		t, ok := remoteaddr.ParseType(e.Type)
		if !ok {
			return remoteaddr.Set{}, fmt.Errorf("addresses[%d]: unknown address type %q", i, e.Type)
		}
		addr, err := remoteaddr.Parse(t, e.Address)
		if err != nil {
			return remoteaddr.Set{}, fmt.Errorf("addresses[%d]: %s", i, err)
		}
		addrs = append(addrs, addr)
	}
	return remoteaddr.NewSet(addrs)
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
		var req remoteAddrsPayload
		if !decodeJSON(w, r, &req) {
			return
		}
		set, perr := parseRemoteAddrSet(req.Addresses)
		if perr != nil {
			http.Error(w, perr.Error(), http.StatusBadRequest)
			return
		}
		err := d.Pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
			return sqlcgen.New(tx).SetPlayerAccountRemoteAddrs(r.Context(), remoteAddrSetParams(acc, set))
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
	set := remoteaddr.SetFromValues(row.RemoteAddrIpLan, row.RemoteAddrIpPublic, row.RemoteAddrDns, row.RemoteAddrIroh)
	entries := []remoteAddrEntry{}
	for _, a := range set.List() {
		entries = append(entries, remoteAddrEntry{Type: string(a.Type), Scope: string(a.Scope), Address: a.Value})
	}
	writeJSON(w, remoteAddrsPayload{Addresses: entries})
}

func remoteAddrSetParams(acc pgtype.UUID, set remoteaddr.Set) sqlcgen.SetPlayerAccountRemoteAddrsParams {
	value := func(a *remoteaddr.Address) *string {
		if a == nil {
			return nil
		}
		return &a.Value
	}
	return sqlcgen.SetPlayerAccountRemoteAddrsParams{
		ID:                 acc,
		RemoteAddrIpLan:    value(set.IPLAN),
		RemoteAddrIpPublic: value(set.IPPublic),
		RemoteAddrDns:      value(set.DNS),
		RemoteAddrIroh:     value(set.Iroh),
	}
}
