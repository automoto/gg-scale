package players

// Player invite acceptance for global accounts. An admin invites an email; the
// magic link delivered to that inbox proves email ownership. Accepting it links
// (or creates) the invitee's GLOBAL gg-scale
// account to the project and signs them in. Invites bypass the public-join
// toggles by design. A tenant-banned account cannot be re-linked via invite.

import (
	"context"
	"errors"
	"net/http"
	"strconv"

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
	CSRFToken   string
	// NewAccount is true when no gg-scale account exists yet for the invited
	// email, so the form must collect a password to create one.
	NewAccount bool
}

var (
	errInviteNotFound = errors.New("players: invite not found")
	errInviteExpired  = errors.New("players: invite expired")
	errInviteBanned   = errors.New("players: account is banned in this tenant")
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
	view.CSRFToken = h.csrf(r)
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
	view.CSRFToken = h.csrf(r)

	var accountID pgtype.UUID
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		// Re-validate the invite inside the tx (SECURITY DEFINER lookup).
		row, qerr := q.PlayerInviteLookup(r.Context(), verifycode.Hash(nil, code))
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

		// Resolve or create the invitee's global account. The invited email is
		// the account email; the magic link proves ownership of it.
		acc, aerr := q.FindAccountIDByEmail(r.Context(), row.Email)
		switch {
		case aerr == nil:
			accountID = acc
		case errors.Is(aerr, pgx.ErrNoRows):
			if !validPlayerPassword(password) {
				return errInviteNeedsPassword
			}
			hash, herr := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
			if herr != nil {
				return herr
			}
			accountID, aerr = q.CreateVerifiedPlayerAccount(r.Context(), sqlcgen.CreateVerifiedPlayerAccountParams{
				Email:        row.Email,
				PasswordHash: hash,
			})
			if aerr != nil {
				return aerr
			}
		default:
			return aerr
		}

		// A tenant-banned account cannot be re-linked via invite.
		if _, berr := q.IsAccountBannedInTenant(r.Context(), sqlcgen.IsAccountBannedInTenantParams{
			TenantID: row.TenantID, PlayerAccountID: accountID,
		}); berr == nil {
			return errInviteBanned
		} else if !errors.Is(berr, pgx.ErrNoRows) {
			return berr
		}

		// Switch into the invite's tenant so RLS admits the player work.
		if _, err := tx.Exec(r.Context(), "SELECT set_config('app.tenant_id', $1, true)", strconv.FormatInt(row.TenantID, 10)); err != nil {
			return err
		}
		emailPtr := &row.Email
		existing, eerr := q.GetPlayerForAccountLink(r.Context(), sqlcgen.GetPlayerForAccountLinkParams{
			ProjectID: projectID, Email: emailPtr,
		})
		switch {
		case eerr == nil:
			if existing.PlayerAccountID.Valid && existing.PlayerAccountID != accountID {
				return errInvitePlayerExists
			}
			if lerr := q.LinkPlayerToAccount(r.Context(), sqlcgen.LinkPlayerToAccountParams{
				ID: existing.ID, PlayerAccountID: accountID,
			}); lerr != nil {
				return lerr
			}
		case errors.Is(eerr, pgx.ErrNoRows):
			externalID, xerr := webutil.RandomHex("user_", 16)
			if xerr != nil {
				return xerr
			}
			if _, cerr := q.CreateLinkedPlayer(r.Context(), sqlcgen.CreateLinkedPlayerParams{
				ProjectID:       projectID,
				ExternalID:      externalID,
				Email:           emailPtr,
				PlayerAccountID: accountID,
			}); cerr != nil {
				if webutil.IsUniqueViolation(cerr) {
					return errInvitePlayerExists
				}
				return cerr
			}
		default:
			return eerr
		}
		return q.MarkPlayerInvitationAccepted(r.Context(), row.ID)
	})
	switch {
	case errors.Is(err, errInviteNotFound), errors.Is(err, errInviteExpired):
		h.renderInviteLookupError(w, r, projectID, err)
		return
	case errors.Is(err, errInviteNeedsPassword):
		view.NewAccount = true
		view.FieldErrors = map[string]string{"password": "Set a password (8–72 characters) to create your account."}
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, InviteAcceptPage(view))
		return
	case errors.Is(err, errInviteBanned):
		view.Error = "This account is banned from this game."
		w.WriteHeader(http.StatusForbidden)
		webutil.Render(r, w, InviteAcceptPage(view))
		return
	case errors.Is(err, errInvitePlayerExists):
		view.Error = "That game profile is already linked to a different account."
		w.WriteHeader(http.StatusConflict)
		webutil.Render(r, w, InviteAcceptPage(view))
		return
	case err != nil:
		webutil.InternalError(w, "player invite: accept", err)
		return
	}

	// Sign the invitee into their global account through the shared 2FA gate
	// so an enrolled account is still challenged for its second factor.
	var epoch int32
	if lerr := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		acc, qerr := sqlcgen.New(tx).GetPlayerAccountByID(r.Context(), accountID)
		if qerr != nil {
			return qerr
		}
		epoch = acc.SessionEpoch
		return nil
	}); lerr != nil {
		webutil.InternalError(w, "player invite: reload", lerr)
		return
	}
	h.finishAccountLogin(w, r, accountID, view.Email, epoch)
}

var (
	errInviteNeedsPassword = errors.New("players: invite needs a password to create account")
	errInvitePlayerExists  = errors.New("players: player already linked to another account")
)

func (h *Handler) lookupPlayerInvite(ctx context.Context, projectID int64, code string) (InviteAcceptView, error) {
	if code == "" {
		return InviteAcceptView{}, errInviteNotFound
	}
	codeHash := verifycode.Hash(nil, code)
	var out InviteAcceptView
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		row, qerr := q.PlayerInviteLookup(ctx, codeHash)
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
		out.ProjectName = row.ProjectName
		// A password field is only needed when no account exists yet.
		if _, aerr := q.FindAccountIDByEmail(ctx, row.Email); errors.Is(aerr, pgx.ErrNoRows) {
			out.NewAccount = true
		} else if aerr != nil {
			return aerr
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
	webutil.Render(r, w, InviteAcceptPage(InviteAcceptView{ProjectID: projectID, Error: msg, CSRFToken: h.csrf(r)}))
}
