package players

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/verifycode"
	"github.com/ggscale/ggscale/internal/webutil"
)

// InviteAcceptView is the data rendered by the player invite-accept page.
type InviteAcceptView struct {
	ProjectID   int64
	Code        string
	Email       string
	ProjectName string
	Error       string
	FieldErrors map[string]string
}

var (
	errInviteNotFound     = errors.New("players: invite not found")
	errInviteExpired      = errors.New("players: invite expired")
	errInvitePlayerExists = errors.New("players: end_user already exists for invite email")
)

func (h *Handler) inviteAcceptPage(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(w, r)
	if !ok {
		return
	}
	code := r.URL.Query().Get("code")
	view, err := h.lookupPlayerInvite(r.Context(), projectID, code)
	if err != nil {
		h.renderInviteLookupError(w, r, projectID, err)
		return
	}
	view.Code = code
	webutil.Render(r, w, InviteAcceptPage(view))
}

func (h *Handler) inviteAcceptHandler(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(w, r)
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	code := r.Form.Get("code")
	password := r.Form.Get("password")

	view, err := h.lookupPlayerInvite(r.Context(), projectID, code)
	if err != nil {
		h.renderInviteLookupError(w, r, projectID, err)
		return
	}
	view.Code = code
	if len(password) < minPlayerPasswordLength {
		view.FieldErrors = map[string]string{"password": "Password must be at least 8 characters."}
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, InviteAcceptPage(view))
		return
	}

	hash, herr := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if herr != nil {
		webutil.InternalError(w, "player invite: bcrypt", herr)
		return
	}
	externalID, exerr := webutil.RandomHex("user_", 16)
	if exerr != nil {
		webutil.InternalError(w, "player invite: external_id", exerr)
		return
	}

	codeHash := verifycode.Hash(nil, code)
	var userID int64
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		// Privileged lookup (SECURITY DEFINER) — needed because the
		// invitation row, its project, and the end_users insert all
		// live behind RLS that requires app.tenant_id, which we only
		// learn from the invite itself.
		row, qerr := q.PlayerInviteLookup(r.Context(), codeHash)
		if errors.Is(qerr, pgx.ErrNoRows) {
			return errInviteNotFound
		}
		if qerr != nil {
			return qerr
		}
		if verifycode.Expired(row.ExpiresAt.Time, h.now()) {
			return errInviteExpired
		}
		if row.ProjectID != projectID {
			return errInviteNotFound
		}
		// Now we can set app.tenant_id and continue under normal RLS.
		if _, err := tx.Exec(r.Context(), "SELECT set_config('app.tenant_id', $1, true)", strconv.FormatInt(row.TenantID, 10)); err != nil {
			return err
		}

		emailPtr := &row.Email
		inserted, cerr := q.CreatePlayerEndUser(r.Context(), sqlcgen.CreatePlayerEndUserParams{
			ProjectID:    row.ProjectID,
			ExternalID:   externalID,
			Email:        emailPtr,
			PasswordHash: hash,
			CodeHash:     nil,
			CodeSalt:     nil,
			ExpiresAt:    pgtype.Timestamptz{},
		})
		if cerr != nil {
			if strings.Contains(cerr.Error(), "23505") {
				return errInvitePlayerExists
			}
			return cerr
		}
		userID = inserted.ID
		// Verified by definition — they had to click the magic link
		// delivered to their inbox.
		if merr := q.MarkPlayerVerified(r.Context(), inserted.ID); merr != nil {
			return merr
		}
		return q.MarkEndUserInvitationAccepted(r.Context(), row.ID)
	})
	switch {
	case errors.Is(err, errInviteNotFound), errors.Is(err, errInviteExpired):
		h.renderInviteLookupError(w, r, projectID, err)
		return
	case errors.Is(err, errInvitePlayerExists):
		view.Error = "An account with that email already exists. Try signing in."
		w.WriteHeader(http.StatusConflict)
		webutil.Render(r, w, InviteAcceptPage(view))
		return
	case err != nil:
		webutil.InternalError(w, "player invite: accept", err)
		return
	}

	if err := h.issueSession(r.Context(), w, userID); err != nil {
		webutil.InternalError(w, "player invite: session", err)
		return
	}
	http.Redirect(w, r, playerAccountPath(projectID), http.StatusSeeOther)
}

func (h *Handler) lookupPlayerInvite(ctx context.Context, projectID int64, code string) (InviteAcceptView, error) {
	if code == "" {
		return InviteAcceptView{}, errInviteNotFound
	}
	codeHash := verifycode.Hash(nil, code)
	var out InviteAcceptView
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).PlayerInviteLookup(ctx, codeHash)
		if errors.Is(qerr, pgx.ErrNoRows) {
			return errInviteNotFound
		}
		if qerr != nil {
			return qerr
		}
		if row.ProjectID != projectID {
			return errInviteNotFound
		}
		if verifycode.Expired(row.ExpiresAt.Time, h.now()) {
			return errInviteExpired
		}
		out.ProjectID = projectID
		out.Email = row.Email
		if row.ProjectName != nil {
			out.ProjectName = *row.ProjectName
		}
		return nil
	})
	if err != nil {
		return InviteAcceptView{}, err
	}
	return out, nil
}

func (h *Handler) renderInviteLookupError(w http.ResponseWriter, r *http.Request, projectID int64, err error) {
	var status int
	var msg string
	switch {
	case errors.Is(err, errInviteExpired):
		status, msg = http.StatusGone, "This invite has expired."
	case errors.Is(err, errInviteNotFound):
		status, msg = http.StatusNotFound, "Invite not found or already used."
	default:
		status, msg = http.StatusInternalServerError, "Could not load invite."
	}
	w.WriteHeader(status)
	webutil.Render(r, w, InviteAcceptPage(InviteAcceptView{ProjectID: projectID, Error: msg}))
}
