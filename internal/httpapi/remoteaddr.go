package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
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
// server-side and ignored on input. type/address are schema-optional so
// parseRemoteAddrSet owns the (indexed, per-entry) 400 validation rather than
// a generic schema 422.
type remoteAddrEntry struct {
	Type    string `json:"type,omitempty"`
	Scope   string `json:"scope,omitempty"`
	Address string `json:"address,omitempty"`
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

type remoteAddrsOutput struct {
	Body remoteAddrsPayload
}

type remoteAddrsPutInput struct {
	Body remoteAddrsPayload
}

type friendRemoteAddrInput struct {
	PlayerID int64 `path:"player_id" minimum:"1"`
}

func registerRemoteAddrRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "getAccountRemoteAddrs",
		Method:      http.MethodGet,
		Path:        "/v1/account/remote-addrs",
		Summary:     "Get the caller's remote addresses",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, ownerRemoteAddrGet(d))

	huma.Register(api, huma.Operation{
		OperationID: "putAccountRemoteAddrs",
		Method:      http.MethodPut,
		Path:        "/v1/account/remote-addrs",
		Summary:     "Replace the caller's remote addresses",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, ownerRemoteAddrPut(d))

	huma.Register(api, huma.Operation{
		OperationID: "getFriendRemoteAddrs",
		Method:      http.MethodGet,
		Path:        "/v1/friends/{player_id}/remote-addrs",
		Summary:     "Get an accepted friend's remote addresses",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, friendRemoteAddrGet(d))
}

// callerAccountForRemoteAddr resolves the caller player's linked account,
// returning a huma 403 when the player is anonymous / unlinked (or the lookup
// fails — a deliberately opaque outcome preserved from the original).
func callerAccountForRemoteAddr(ctx context.Context, d Deps) (pgtype.UUID, error) {
	playerID, ok := playerauth.IDFromContext(ctx)
	if !ok {
		return pgtype.UUID{}, huma.Error401Unauthorized("no player")
	}
	var acc pgtype.UUID
	err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
		var e error
		acc, e = sqlcgen.New(tx).GetPlayerLinkedAccountID(ctx, playerID)
		return e
	})
	if err != nil || !acc.Valid {
		return pgtype.UUID{}, huma.Error403Forbidden(linkAccountMsg)
	}
	return acc, nil
}

func ownerRemoteAddrGet(d Deps) func(context.Context, *struct{}) (*remoteAddrsOutput, error) {
	return func(ctx context.Context, _ *struct{}) (*remoteAddrsOutput, error) {
		acc, err := callerAccountForRemoteAddr(ctx, d)
		if err != nil {
			return nil, err
		}
		payload, rerr := readRemoteAddrs(ctx, d, acc)
		if rerr != nil {
			return nil, serverError(ctx, "remote-addr read", rerr)
		}
		return &remoteAddrsOutput{Body: payload}, nil
	}
}

func ownerRemoteAddrPut(d Deps) func(context.Context, *remoteAddrsPutInput) (*remoteAddrsOutput, error) {
	return func(ctx context.Context, in *remoteAddrsPutInput) (*remoteAddrsOutput, error) {
		acc, err := callerAccountForRemoteAddr(ctx, d)
		if err != nil {
			return nil, err
		}
		set, perr := parseRemoteAddrSet(in.Body.Addresses)
		if perr != nil {
			return nil, huma.Error400BadRequest(perr.Error())
		}
		if werr := d.Pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
			return sqlcgen.New(tx).SetPlayerAccountRemoteAddrs(ctx, remoteAddrSetParams(acc, set))
		}); werr != nil {
			return nil, serverError(ctx, "remote-addr put", werr)
		}
		payload, rerr := readRemoteAddrs(ctx, d, acc)
		if rerr != nil {
			return nil, serverError(ctx, "remote-addr read", rerr)
		}
		return &remoteAddrsOutput{Body: payload}, nil
	}
}

// friendRemoteAddrGet lets an ACCEPTED friend read the target's remote
// addresses. Non-friends, blocked players, and unlinked callers are denied,
// and a non-friend is not distinguished from a blocker.
func friendRemoteAddrGet(d Deps) func(context.Context, *friendRemoteAddrInput) (*remoteAddrsOutput, error) {
	return func(ctx context.Context, in *friendRemoteAddrInput) (*remoteAddrsOutput, error) {
		me, err := callerAccountForRemoteAddr(ctx, d)
		if err != nil {
			return nil, err
		}
		targetPlayer := in.PlayerID
		var targetAcc pgtype.UUID
		qerr := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			acc, e := q.GetPlayerLinkedAccountID(ctx, targetPlayer)
			if e != nil {
				return e
			}
			if !acc.Valid {
				return errTargetNoAccount
			}
			targetAcc = acc
			// Must be accepted friends AND not blocked in either direction.
			if _, be := q.IsBlockedBetweenAccounts(ctx, sqlcgen.IsBlockedBetweenAccountsParams{A: me, B: targetAcc}); be == nil {
				return errFriendBlocked
			} else if !errors.Is(be, pgx.ErrNoRows) {
				return be
			}
			if _, fe := q.AreAccountsFriendsAccepted(ctx, sqlcgen.AreAccountsFriendsAcceptedParams{A: me, B: targetAcc}); fe != nil {
				if errors.Is(fe, pgx.ErrNoRows) {
					return errNotFriends
				}
				return fe
			}
			return nil
		})
		switch {
		case errors.Is(qerr, errTargetNoAccount), errors.Is(qerr, pgx.ErrNoRows):
			return nil, huma.Error404NotFound("not found")
		case errors.Is(qerr, errNotFriends), errors.Is(qerr, errFriendBlocked):
			return nil, huma.Error403Forbidden("forbidden")
		case qerr != nil:
			return nil, serverError(ctx, "friend remote-addr", qerr)
		}
		payload, rerr := readRemoteAddrs(ctx, d, targetAcc)
		if rerr != nil {
			return nil, serverError(ctx, "remote-addr read", rerr)
		}
		return &remoteAddrsOutput{Body: payload}, nil
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

// readRemoteAddrs loads an account's stored address set as a wire payload.
func readRemoteAddrs(ctx context.Context, d Deps, acc pgtype.UUID) (remoteAddrsPayload, error) {
	var row sqlcgen.GetPlayerAccountRemoteAddrsRow
	if err := d.Pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var e error
		row, e = sqlcgen.New(tx).GetPlayerAccountRemoteAddrs(ctx, acc)
		return e
	}); err != nil {
		return remoteAddrsPayload{}, err
	}
	set := remoteaddr.SetFromValues(row.RemoteAddrIpLan, row.RemoteAddrIpPublic, row.RemoteAddrDns, row.RemoteAddrIroh)
	entries := []remoteAddrEntry{}
	for _, a := range set.List() {
		entries = append(entries, remoteAddrEntry{Type: string(a.Type), Scope: string(a.Scope), Address: a.Value})
	}
	return remoteAddrsPayload{Addresses: entries}, nil
}

// writeRemoteAddrs serves an account's addresses over the still-chi server-tier
// route.
func writeRemoteAddrs(w http.ResponseWriter, d Deps, r *http.Request, acc pgtype.UUID) {
	payload, err := readRemoteAddrs(r.Context(), d, acc)
	if err != nil {
		webutil.InternalError(w, "remote-addr read", err)
		return
	}
	writeJSON(w, payload)
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
