package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/verifycode"
	"github.com/ggscale/ggscale/internal/webutil"
)

const playersPerPage = 25

const playerInviteSubject = "You've been invited to play"

type playerInviteResult struct {
	ID        int64
	Email     string
	Code      string
	ExpiresAt time.Time
}

// errProjectNotInTenant is returned when the URL projectID does not
// belong to the URL tenantID — guards against tenant-A admins crafting
// invites against tenant-B projects.
var errProjectNotInTenant = errors.New("dashboard: project not in tenant")

// createPlayerInvite mints a code, persists the row (privileged path),
// and returns the plaintext code so the caller can email it.
func (h *Handler) createPlayerInvite(ctx context.Context, tenantID, projectID int64, email string, invitedBy int64) (playerInviteResult, error) {
	code, err := verifycode.GenerateInviteCode()
	if err != nil {
		return playerInviteResult{}, fmt.Errorf("invite code: %w", err)
	}
	codeHash := verifycode.Hash(nil, code)
	expiresAt := h.now().Add(verifycode.InviteTTL)

	var row sqlcgen.CreateEndUserInvitationRow
	err = h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		// Set app.tenant_id first so RLS on projects + end_user_invitations
		// admits both the ownership check and the insert.
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", stringFromInt(tenantID)); err != nil {
			return fmt.Errorf("set app.tenant_id: %w", err)
		}
		// Defense-in-depth: confirm the project actually belongs to the
		// route's tenant before inserting. RLS already filters projects
		// to the set tenant_id, so ErrNoRows here means "project not
		// in this tenant"; the explicit TenantID equality is paranoia
		// for the day someone tightens RLS to a different rule.
		proj, perr := q.GetProjectTenant(ctx, projectID)
		if errors.Is(perr, pgx.ErrNoRows) {
			return errProjectNotInTenant
		}
		if perr != nil {
			return perr
		}
		if proj.TenantID != tenantID {
			return errProjectNotInTenant
		}
		r, qerr := q.CreateEndUserInvitation(ctx, sqlcgen.CreateEndUserInvitationParams{
			ProjectID:       projectID,
			Email:           email,
			CodeHash:        codeHash,
			ExpiresAt:       pgtype.Timestamptz{Time: expiresAt, Valid: true},
			InvitedByUserID: invitedBy,
		})
		if qerr != nil {
			return qerr
		}
		row = r
		return nil
	})
	if err != nil {
		return playerInviteResult{}, err
	}
	return playerInviteResult{
		ID:        row.ID,
		Email:     email,
		Code:      code,
		ExpiresAt: row.ExpiresAt.Time,
	}, nil
}

// sendPlayerInviteEmail mails the invite recipient a magic link into the
// player site. Failure is logged but does not block the request.
func (h *Handler) sendPlayerInviteEmail(ctx context.Context, res playerInviteResult, projectID int64) {
	if h.mailer == nil || h.cfg.MailFrom == "" {
		slog.WarnContext(ctx, "player invite: no mailer configured", "invite_id", res.ID, "email", res.Email)
		return
	}
	base := strings.TrimRight(h.cfg.BaseURL, "/")
	link := base + "/v1/players/p/" + strconv.FormatInt(projectID, 10) + "/invite/accept?code=" + url.QueryEscape(res.Code)
	body := fmt.Sprintf("You were invited to play. Click to set up your account (expires %s):\n%s",
		res.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"), link)
	if err := h.mailer.Send(ctx, mailer.Message{
		From:    h.cfg.MailFrom,
		To:      []string{res.Email},
		Subject: playerInviteSubject,
		Body:    body,
	}); err != nil {
		slog.ErrorContext(ctx, "player invite mailer", "err", err, "invite_id", res.ID)
	}
}

// PlayerView is one row in the player list/detail page.
type PlayerView struct {
	ID              int64
	ExternalID      string
	Email           string
	EmailVerifiedAt time.Time
	DisabledAt      time.Time
	CreatedAt       time.Time
}

// PlayersView is the data rendered by the players list page.
type PlayersView struct {
	UserEmail string
	CSRFToken string
	TenantID  int64
	ProjectID int64
	Search    string
	Players   []PlayerView
	Total     int64
	Page      int
	HasPrev   bool
	HasNext   bool
	Message   string
}

// PlayerDetailView is the data rendered by the per-player detail page.
type PlayerDetailView struct {
	UserEmail string
	CSRFToken string
	TenantID  int64
	ProjectID int64
	Player    PlayerView
	Message   string
}

func (h *Handler) playersListPage(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	projectID, ok := parsePathID(w, r, "projectID")
	if !ok {
		return
	}
	search := r.URL.Query().Get("q")
	page := pageParam(r)
	offset := dashboardPageOffset(page, playersPerPage)

	var (
		players []PlayerView
		total   int64
		hasNext bool
	)
	// Use Q + WithTenant so RLS enforces tenant isolation as defense-in-depth.
	// The query still passes TenantID — both layers must agree on which
	// tenant the request is for, so a forgotten WHERE clause downstream
	// can't leak rows across tenants.
	ctx := db.WithTenant(r.Context(), tenantID)
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var filter *string
		if search != "" {
			filter = &search
		}
		rows, err := q.ListPlayersForProject(ctx, sqlcgen.ListPlayersForProjectParams{
			TenantID:    tenantID,
			ProjectID:   projectID,
			EmailFilter: filter,
			Lim:         dashboardPageLimit(playersPerPage),
			Off:         offset,
		})
		if err != nil {
			return err
		}
		if len(rows) > playersPerPage {
			hasNext = true
			rows = rows[:playersPerPage]
		}
		for _, row := range rows {
			pv := PlayerView{
				ID:         row.ID,
				ExternalID: row.ExternalID,
				CreatedAt:  row.CreatedAt.Time,
			}
			pv.Email = row.Email
			pv.EmailVerifiedAt = row.EmailVerifiedAt.Time
			pv.DisabledAt = row.DisabledAt.Time
			players = append(players, pv)
		}
		total = int64(offset) + int64(len(players))
		if hasNext {
			total++
		}
		return nil
	})
	if err != nil {
		http.Error(w, "player list failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, PlayersPage(PlayersView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		TenantID:  tenantID,
		ProjectID: projectID,
		Search:    search,
		Players:   players,
		Total:     total,
		Page:      page,
		HasPrev:   page > 1,
		HasNext:   hasNext,
		Message:   r.URL.Query().Get("flash"),
	}))

}

func (h *Handler) playerDetailPage(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	projectID, ok := parsePathID(w, r, "projectID")
	if !ok {
		return
	}
	playerID, ok := parsePathID(w, r, "playerID")
	if !ok {
		return
	}
	ctx := db.WithTenant(r.Context(), tenantID)
	var row sqlcgen.GetPlayerForProjectRow
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).GetPlayerForProject(ctx, sqlcgen.GetPlayerForProjectParams{
			TenantID:  tenantID,
			ProjectID: projectID,
			ID:        playerID,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "player lookup failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, PlayerDetailPage(PlayerDetailView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		TenantID:  tenantID,
		ProjectID: projectID,
		Player: PlayerView{
			ID:              row.ID,
			ExternalID:      row.ExternalID,
			Email:           row.Email,
			EmailVerifiedAt: row.EmailVerifiedAt.Time,
			DisabledAt:      row.DisabledAt.Time,
			CreatedAt:       row.CreatedAt.Time,
		},
		Message: r.URL.Query().Get("flash"),
	}))

}

func (h *Handler) invitePlayerPage(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	projectID, ok := parsePathID(w, r, "projectID")
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, InvitePlayerPage(InvitePlayerView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		TenantID:  tenantID,
		ProjectID: projectID,
	}))

}

func (h *Handler) invitePlayerHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	projectID, ok := parsePathID(w, r, "projectID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	email := normalizeEmail(r.Form.Get("email"))
	if !validDashboardEmail(email) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, InvitePlayerPage(InvitePlayerView{
			UserEmail:   session.User.Email,
			CSRFToken:   session.CSRFToken,
			TenantID:    tenantID,
			ProjectID:   projectID,
			Email:       email,
			FieldErrors: map[string]string{"email": "Enter a valid email."},
		}))

		return
	}
	res, err := h.createPlayerInvite(r.Context(), tenantID, projectID, email, session.User.ID)
	if err != nil {
		view := InvitePlayerView{
			UserEmail: session.User.Email, CSRFToken: session.CSRFToken,
			TenantID: tenantID, ProjectID: projectID, Email: email,
		}
		switch {
		case errors.Is(err, errProjectNotInTenant):
			view.Error = "That project does not belong to this tenant."
			w.WriteHeader(http.StatusNotFound)
		case isUniqueViolation(err):
			view.Error = "An invite for that email is already pending."
			w.WriteHeader(http.StatusConflict)
		default:
			slog.ErrorContext(r.Context(), "player invite: create", "err", err)
			view.Error = "Invite could not be sent."
			w.WriteHeader(http.StatusInternalServerError)
		}
		webutil.Render(r, w, InvitePlayerPage(view))
		return
	}
	h.sendPlayerInviteEmail(r.Context(), res, projectID)
	target := pathTenantsPrefix + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) +
		"/players?flash=" + url.QueryEscape("Invite sent to "+res.Email)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *Handler) playerToggleDisableHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	projectID, ok := parsePathID(w, r, "projectID")
	if !ok {
		return
	}
	playerID, ok := parsePathID(w, r, "playerID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	enable := r.Form.Get("enable") == "true"
	var disabledAt pgtype.Timestamptz
	if !enable {
		disabledAt = pgtype.Timestamptz{Time: h.now(), Valid: true}
	}
	ctx := db.WithTenant(r.Context(), tenantID)
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).SetPlayerDisabledByTenant(ctx, sqlcgen.SetPlayerDisabledByTenantParams{
			ID:         playerID,
			ProjectID:  projectID,
			TenantID:   tenantID,
			DisabledAt: disabledAt,
		})
	})
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	flash := "Player disabled."
	if enable {
		flash = "Player re-enabled."
	}
	target := pathTenantsPrefix + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) +
		"/players/" + strconv.FormatInt(playerID, 10) + queryFlash + url.QueryEscape(flash)
	htmxRedirect(w, r, target)
}
