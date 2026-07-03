package players

// Player-site friends UI, operating on the global account session. Friends are
// account-to-account (see docs/temp/player-accounts.md). Actions are plain
// form POSTs that redirect back to the friends page — matching the rest of the
// player site, which does not load HTMX/JS assets.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/webutil"
)

const friendsPath = accountBasePath + "/friends"

// friendRequestSentFlash is shown whether or not the target exists, so the
// form can't be used to enumerate which emails / display names have an account.
const friendRequestSentFlash = "If an account matches, a friend request was sent."

var (
	errFriendTargetNotFound = errors.New("players: no account matches that email or name")
	errFriendAmbiguous      = errors.New("players: multiple accounts share that display name — use an email")
	errFriendSelfTarget     = errors.New("players: cannot friend yourself")
)

func (h *Handler) friendsPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	view := FriendsView{CSRFToken: h.csrf(r), Flash: r.URL.Query().Get("flash"), FlashError: r.URL.Query().Get("error")}
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var e error
		view.Accepted, e = h.loadFriendGroup(r.Context(), tx, sess.AccountID, "accepted")
		if e != nil {
			return e
		}
		view.IncomingPending, e = h.loadIncomingPending(r.Context(), tx, sess.AccountID)
		if e != nil {
			return e
		}
		view.OutgoingPending, e = h.loadOutgoingPending(r.Context(), tx, sess.AccountID)
		if e != nil {
			return e
		}
		view.Blocked, e = h.loadFriendGroup(r.Context(), tx, sess.AccountID, "blocked")
		return e
	})
	if err != nil {
		webutil.InternalError(w, "friends page", err)
		return
	}
	webutil.Render(r, w, FriendsPage(view))
}

// loadFriendGroup returns the "other" account in each edge of the given status
// (accepted/blocked), enriched with email + display name.
func (h *Handler) loadFriendGroup(ctx context.Context, tx pgx.Tx, me uuid.UUID, status string) ([]FriendRow, error) {
	q := sqlcgen.New(tx)
	mePg := toPgUUID(me)
	rows, err := q.ListFriendsByStatusForAccount(ctx, sqlcgen.ListFriendsByStatusForAccountParams{
		Me: mePg, Status: status, Cursor: 0, RowLimit: 500,
	})
	if err != nil {
		return nil, err
	}
	return h.enrichEdges(ctx, tx, mePg, rows)
}

func (h *Handler) loadIncomingPending(ctx context.Context, tx pgx.Tx, me uuid.UUID) ([]FriendRow, error) {
	// Incoming = pending edges where I am the "to" side.
	q := sqlcgen.New(tx)
	mePg := toPgUUID(me)
	rows, err := q.ListFriendsByStatusForAccount(ctx, sqlcgen.ListFriendsByStatusForAccountParams{
		Me: mePg, Status: "pending", Cursor: 0, RowLimit: 500,
	})
	if err != nil {
		return nil, err
	}
	var incoming []sqlcgen.FriendEdge
	for _, row := range rows {
		if row.ToAccountID == mePg {
			incoming = append(incoming, row)
		}
	}
	return h.enrichEdges(ctx, tx, mePg, incoming)
}

func (h *Handler) loadOutgoingPending(ctx context.Context, tx pgx.Tx, me uuid.UUID) ([]FriendRow, error) {
	q := sqlcgen.New(tx)
	mePg := toPgUUID(me)
	rows, err := q.ListFriendsByStatusForAccount(ctx, sqlcgen.ListFriendsByStatusForAccountParams{
		Me: mePg, Status: "pending", Cursor: 0, RowLimit: 500,
	})
	if err != nil {
		return nil, err
	}
	var outgoing []sqlcgen.FriendEdge
	for _, row := range rows {
		if row.FromAccountID == mePg {
			outgoing = append(outgoing, row)
		}
	}
	return h.enrichEdges(ctx, tx, mePg, outgoing)
}

func (h *Handler) enrichEdges(ctx context.Context, tx pgx.Tx, me pgtype.UUID, rows []sqlcgen.FriendEdge) ([]FriendRow, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	q := sqlcgen.New(tx)
	others := make([]pgtype.UUID, 0, len(rows))
	for _, row := range rows {
		other := row.ToAccountID
		if other == me {
			other = row.FromAccountID
		}
		others = append(others, other)
	}
	idRows, err := q.ListAccountIdentities(ctx, others)
	if err != nil {
		return nil, err
	}
	idMap := map[pgtype.UUID]sqlcgen.ListAccountIdentitiesRow{}
	for _, ir := range idRows {
		idMap[ir.ID] = ir
	}
	out := make([]FriendRow, 0, len(rows))
	for i := range rows {
		fr := FriendRow{AccountID: uuid.UUID(others[i].Bytes).String()}
		if ir, ok := idMap[others[i]]; ok {
			fr.Email = ir.Email
			if ir.DisplayName != nil {
				fr.DisplayName = *ir.DisplayName
			}
		}
		out = append(out, fr)
	}
	return out, nil
}

// POST /account/friends/request — by email or display name.
func (h *Handler) friendRequest(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	target := strings.TrimSpace(r.Form.Get("target"))
	if target == "" {
		h.redirectFriends(w, r, "", "Enter an email or display name.")
		return
	}
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		toAcc, rerr := h.resolveFriendTarget(r.Context(), tx, target)
		if rerr != nil {
			return rerr
		}
		mePg := toPgUUID(sess.AccountID)
		if toAcc == mePg {
			return errFriendSelfTarget
		}
		if _, berr := q.IsBlockedBetweenAccounts(r.Context(), sqlcgen.IsBlockedBetweenAccountsParams{A: mePg, B: toAcc}); berr == nil {
			return errFriendTargetNotFound // don't reveal the block
		} else if !errors.Is(berr, pgx.ErrNoRows) {
			return berr
		}
		if _, rerr := q.RequestFriendByAccount(r.Context(), sqlcgen.RequestFriendByAccountParams{
			FromAccountID: mePg, ToAccountID: toAcc,
		}); rerr != nil && !errors.Is(rerr, pgx.ErrNoRows) {
			return rerr
		}
		return nil
	})
	switch {
	case errors.Is(err, errFriendTargetNotFound):
		// Uniform with the success path so a caller can't probe which emails /
		// display names have an account (and a block stays hidden).
		h.redirectFriends(w, r, friendRequestSentFlash, "")
	case errors.Is(err, errFriendAmbiguous):
		h.redirectFriends(w, r, "", "Multiple accounts share that display name — use an email.")
	case errors.Is(err, errFriendSelfTarget):
		h.redirectFriends(w, r, "", "You can't friend yourself.")
	case err != nil:
		webutil.InternalError(w, "friend request", err)
	default:
		h.redirectFriends(w, r, friendRequestSentFlash, "")
	}
}

func (h *Handler) resolveFriendTarget(ctx context.Context, tx pgx.Tx, target string) (pgtype.UUID, error) {
	q := sqlcgen.New(tx)
	if strings.Contains(target, "@") {
		id, err := q.FindAccountIDByEmail(ctx, strings.ToLower(target))
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, errFriendTargetNotFound
		}
		return id, err
	}
	ids, err := q.FindAccountIDsByDisplayName(ctx, ptrString(target))
	if err != nil {
		return pgtype.UUID{}, err
	}
	switch len(ids) {
	case 0:
		return pgtype.UUID{}, errFriendTargetNotFound
	case 1:
		return ids[0].ID, nil
	default:
		return pgtype.UUID{}, errFriendAmbiguous
	}
}

// POST /account/friends/{accountID}/{action}
func (h *Handler) friendAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := h.accountSessionFromRequest(r)
		if !ok {
			http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
			return
		}
		if !webutil.ParseForm(w, r) {
			return
		}
		otherID, perr := uuid.Parse(chi.URLParam(r, "accountID"))
		if perr != nil {
			h.redirectFriends(w, r, "", "Unknown account.")
			return
		}
		me := toPgUUID(sess.AccountID)
		other := toPgUUID(otherID)
		if me == other {
			h.redirectFriends(w, r, "", "That action isn't allowed.")
			return
		}
		flash, err := h.applyFriendAction(r.Context(), action, me, other)
		if err != nil {
			webutil.InternalError(w, "friend action", err)
			return
		}
		h.redirectFriends(w, r, flash, "")
	}
}

func (h *Handler) applyFriendAction(ctx context.Context, action string, me, other pgtype.UUID) (string, error) {
	var flash string
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		switch action {
		case "accept", "reject":
			// Only a pending incoming request (other→me) may be accepted or
			// rejected. Reading the current status first prevents flipping a
			// 'blocked' or 'rejected' edge — the JSON path enforces the same
			// guard, and without it a blocked user could turn the block into a
			// friendship.
			edge, err := q.GetFriendEdgeByAccount(ctx, sqlcgen.GetFriendEdgeByAccountParams{
				FromAccountID: other, ToAccountID: me,
			})
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && edge.Status != "pending") {
				flash = "That request is no longer available."
				return nil
			}
			if err != nil {
				return err
			}
			newStatus := "accepted"
			flash = "Friend added."
			if action == "reject" {
				newStatus = "rejected"
				flash = "Request declined."
			}
			return q.SetFriendEdgeStatusByAccount(ctx, sqlcgen.SetFriendEdgeStatusByAccountParams{
				FromAccountID: other, ToAccountID: me, Status: newStatus,
			})
		case "unfriend":
			flash = "Removed."
			_, e := q.DeleteFriendEdgeByAccount(ctx, sqlcgen.DeleteFriendEdgeByAccountParams{Me: me, Other: other})
			return e
		case "block":
			flash = "Blocked."
			_, _ = q.DeleteFriendEdgeDirected(ctx, sqlcgen.DeleteFriendEdgeDirectedParams{
				FromAccountID: other, ToAccountID: me, Status: "accepted",
			})
			_, _ = q.DeleteFriendEdgeDirected(ctx, sqlcgen.DeleteFriendEdgeDirectedParams{
				FromAccountID: other, ToAccountID: me, Status: "pending",
			})
			return q.UpsertFriendEdgeStatusByAccount(ctx, sqlcgen.UpsertFriendEdgeStatusByAccountParams{
				FromAccountID: me, ToAccountID: other, Status: "blocked",
			})
		case "unblock":
			flash = "Unblocked."
			_, e := q.DeleteFriendEdgeDirected(ctx, sqlcgen.DeleteFriendEdgeDirectedParams{
				FromAccountID: me, ToAccountID: other, Status: "blocked",
			})
			return e
		default:
			flash = ""
			return nil
		}
	})
	return flash, err
}

func (h *Handler) redirectFriends(w http.ResponseWriter, r *http.Request, flash, flashErr string) {
	target := friendsPath
	switch {
	case flash != "":
		target += "?flash=" + url.QueryEscape(flash)
	case flashErr != "":
		target += "?error=" + url.QueryEscape(flashErr)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func ptrString(s string) *string { return &s }
