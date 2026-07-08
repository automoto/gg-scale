package controlpanel

// Platform-admin view over global player_accounts. Accounts are
// platform-global, so every query runs through BootstrapQ. Gated by
// requirePlatformAdmin at the router.

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ggscale/ggscale/internal/auditlog"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/webutil"
)

const playerAccountsSearchLimit = 50

func parseAccountID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		http.Error(w, "invalid account id", http.StatusBadRequest)
		return uuid.UUID{}, false
	}
	return id, true
}

func (h *Handler) platformPlayerAccountsPage(w http.ResponseWriter, r *http.Request) {
	search := r.URL.Query().Get("q")

	var rows []sqlcgen.SearchPlayerAccountsRow
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var qerr error
		rows, qerr = sqlcgen.New(tx).SearchPlayerAccounts(r.Context(), sqlcgen.SearchPlayerAccountsParams{
			Query:    search,
			RowLimit: playerAccountsSearchLimit,
		})
		return qerr
	})
	if err != nil {
		webutil.InternalError(w, "player accounts: search", err)
		return
	}

	session, _ := sessionFromContext(r.Context())
	accounts := make([]PlayerAccountView, 0, len(rows))
	for _, row := range rows {
		accounts = append(accounts, PlayerAccountView{
			ID:         uuid.UUID(row.ID.Bytes).String(),
			Email:      row.Email,
			Verified:   row.EmailVerifiedAt.Valid,
			Disabled:   row.DisabledAt.Valid,
			CreatedAt:  row.CreatedAt.Time,
			HasDisplay: row.DisplayName != nil,
		})
	}
	webutil.Render(r, w, PlayerAccountsPage(PlayerAccountsView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		Search:    search,
		Accounts:  accounts,
		Message:   r.URL.Query().Get("flash"),
	}))
}

func (h *Handler) platformPlayerAccountDetailPage(w http.ResponseWriter, r *http.Request) {
	accountID, ok := parseAccountID(w, r)
	if !ok {
		return
	}
	pgID := pgtype.UUID{Bytes: accountID, Valid: true}

	var (
		acc      sqlcgen.GetPlayerAccountByIDRow
		projects []LinkedProjectView
	)
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var qerr error
		acc, qerr = sqlcgen.New(tx).GetPlayerAccountByID(r.Context(), pgID)
		if qerr != nil {
			return qerr
		}
		linkRows, lerr := tx.Query(r.Context(),
			`SELECT tenant_id, project_id, project_name, external_id
			 FROM player_account_linked_projects($1)`, pgID)
		if lerr != nil {
			return lerr
		}
		defer linkRows.Close()
		for linkRows.Next() {
			var lp LinkedProjectView
			if scanErr := linkRows.Scan(&lp.TenantID, &lp.ProjectID, &lp.ProjectName, &lp.ExternalID); scanErr != nil {
				return scanErr
			}
			projects = append(projects, lp)
		}
		return linkRows.Err()
	})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		webutil.InternalError(w, "player account: detail", err)
		return
	}

	session, _ := sessionFromContext(r.Context())
	view := PlayerAccountDetailView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		ID:        accountID.String(),
		Email:     acc.Email,
		Verified:  acc.EmailVerifiedAt.Valid,
		Disabled:  acc.DisabledAt.Valid,
		CreatedAt: acc.CreatedAt.Time,
		Projects:  projects,
		Message:   r.URL.Query().Get("flash"),
	}
	if acc.DisplayName != nil {
		view.DisplayName = *acc.DisplayName
	}
	webutil.Render(r, w, PlayerAccountDetailPage(view))
}

func (h *Handler) disablePlayerAccountHandler(w http.ResponseWriter, r *http.Request) {
	h.togglePlayerAccountDisabled(w, r, true)
}

func (h *Handler) enablePlayerAccountHandler(w http.ResponseWriter, r *http.Request) {
	h.togglePlayerAccountDisabled(w, r, false)
}

func (h *Handler) togglePlayerAccountDisabled(w http.ResponseWriter, r *http.Request, disable bool) {
	accountID, ok := parseAccountID(w, r)
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	pgID := pgtype.UUID{Bytes: accountID, Valid: true}
	session, _ := sessionFromContext(r.Context())

	action := "control_panel.player_account.enabled"
	flashMsg := "Account re-enabled."
	if disable {
		action = "control_panel.player_account.disabled"
		flashMsg = "Account disabled platform-wide. Sessions revoked."
	}

	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		acc, err := q.GetPlayerAccountByID(r.Context(), pgID)
		if err != nil {
			return err
		}
		if disable {
			// SetPlayerAccountDisabled bumps session_epoch; revoke the stored
			// sessions too so the row set is clean.
			if err := q.SetPlayerAccountDisabled(r.Context(), pgID); err != nil {
				return err
			}
			if err := q.RevokeAllPlayerAccountSessions(r.Context(), pgID); err != nil {
				return err
			}
		} else if err := q.SetPlayerAccountEnabled(r.Context(), pgID); err != nil {
			return err
		}
		return auditlog.WritePlatform(r.Context(), tx, session.User.ID, action,
			"player_account:"+accountID.String(),
			map[string]any{"player_account_id": accountID.String(), "email": acc.Email})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		webutil.InternalError(w, "player account toggle", err)
		return
	}
	htmxRedirect(w, r, pathAdminAccounts+"/"+accountID.String()+queryFlash+url.QueryEscape(flashMsg))
}
