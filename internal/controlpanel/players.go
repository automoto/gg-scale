package controlpanel

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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/automoto/gg-scale/internal/auditlog"
	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/mailer"
	"github.com/automoto/gg-scale/internal/observability"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/remoteaddr"
	"github.com/automoto/gg-scale/internal/verifycode"
	"github.com/automoto/gg-scale/internal/webutil"
)

const playersPerPage = 25

type playerInviteResult struct {
	ID        int64
	Email     string
	Code      string
	ExpiresAt time.Time
}

// errProjectNotInTenant is returned when the URL projectID does not
// belong to the URL tenantID — guards against tenant-A admins crafting
// invites against tenant-B projects.
var errProjectNotInTenant = errors.New("control panel: project not in tenant")

// errPlayerEmailTaken means the email is already owned by a different player in
// the project, so it can't be bound onto the "link player" target row.
var errPlayerEmailTaken = errors.New("control panel: email already used by another player")

// createPlayerInvite mints a code, persists the row (privileged path), and
// returns the plaintext code so the caller can email it. A non-nil targetPlayer
// makes this an admin "link player" invite: acceptance binds the proven email
// onto that existing row, and any prior open invite for it is superseded.
func (h *Handler) createPlayerInvite(ctx context.Context, tenantID, projectID int64, email string, invitedBy int64, targetPlayer *int64) (playerInviteResult, error) {
	code, err := verifycode.GenerateInviteCode()
	if err != nil {
		return playerInviteResult{}, fmt.Errorf("invite code: %w", err)
	}
	codeHash := verifycode.Hash(nil, code)
	expiresAt := h.now().Add(verifycode.InviteTTL)

	var row sqlcgen.CreatePlayerInvitationRow
	err = h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		// Set app.tenant_id first so RLS on projects + player_invitations
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
		if targetPlayer != nil {
			// Reject if the email already belongs to a different player: the
			// email unique index would block the bind on accept anyway, and a
			// clear up-front error beats a late conflict.
			existing, eerr := q.GetPlayerForAccountLink(ctx, sqlcgen.GetPlayerForAccountLinkParams{
				ProjectID: projectID, Email: &email,
			})
			switch {
			case eerr == nil && existing.ID != *targetPlayer:
				return errPlayerEmailTaken
			case eerr != nil && !errors.Is(eerr, pgx.ErrNoRows):
				return eerr
			}
			// Supersede any prior open invite that would collide with this one —
			// keyed on the target row or the (project_id, email) open-invite
			// unique index — so the resend isn't rejected by that index.
			if rerr := q.RevokeSupersededPlayerInvitations(ctx, sqlcgen.RevokeSupersededPlayerInvitationsParams{
				ProjectID:       projectID,
				ProjectPlayerID: targetPlayer,
				Email:           email,
			}); rerr != nil {
				return rerr
			}
		}
		r, qerr := q.CreatePlayerInvitation(ctx, sqlcgen.CreatePlayerInvitationParams{
			ProjectID:       projectID,
			Email:           email,
			CodeHash:        codeHash,
			ExpiresAt:       pgtype.Timestamptz{Time: expiresAt, Valid: true},
			InvitedByUserID: invitedBy,
			ProjectPlayerID: targetPlayer,
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

// errInviteThrottled marks a send rejected by the per-recipient/inviter/domain
// throttle so the caller can render its own 429 with the retry hint.
var errInviteThrottled = errors.New("control panel: invite throttled")

// createAndSendPlayerInvite is the throttle → create → refund-on-error →
// metrics → email pipeline shared by the invite-player and link-player
// handlers. targetPlayer is nil for a plain invite or the row id for a link
// invite. On a throttle rejection it returns errInviteThrottled with the
// retry-after seconds; on a create failure it refunds the debited token and
// returns the underlying error. How the error is presented (full page vs
// dialog fragment) is left to the caller.
func (h *Handler) createAndSendPlayerInvite(r *http.Request, tenantID, projectID, inviterID int64, email string, targetPlayer *int64) (playerInviteResult, int, error) {
	if retry, throttled := h.inviteThrottled(r.Context(), inviterID, tenantID, projectID, email); throttled {
		return playerInviteResult{}, retry, errInviteThrottled
	}
	res, err := h.createPlayerInvite(r.Context(), tenantID, projectID, email, inviterID, targetPlayer)
	if err != nil {
		// The throttle already debited this send; the invite didn't happen, so
		// return the tokens rather than charging a failed attempt against quota.
		h.inviteRefund(r.Context(), inviterID, tenantID, projectID, email)
		return playerInviteResult{}, 0, err
	}
	h.metrics.InviteSent(observability.InvitePlayer)
	session, _ := sessionFromContext(r.Context())
	h.sendPlayerInviteEmail(r.Context(), res, tenantID, projectID, session.User.Email)
	return res, 0, nil
}

// sendPlayerInviteEmail mails the invite recipient a magic link into the
// player site. Failure is logged but does not block the request. Suppressed
// (unsubscribed) addresses are dropped; the invite itself still exists.
func (h *Handler) sendPlayerInviteEmail(ctx context.Context, res playerInviteResult, tenantID, projectID int64, inviterEmail string) {
	if h.mailer == nil || h.cfg.MailFrom == "" {
		slog.WarnContext(ctx, "player invite: no mailer configured", "invite_id", res.ID, "email", res.Email)
		return
	}
	if h.inviteEmailSuppressed(ctx, res.Email) {
		return
	}
	gameName := h.projectDisplayName(ctx, tenantID, projectID)
	base := strings.TrimRight(h.cfg.BaseURL, "/")
	link := base + "/v1/players/p/" + strconv.FormatInt(projectID, 10) + "/invite/accept?code=" + url.QueryEscape(res.Code)
	unsubscribe := webutil.EmailUnsubscribeURL(h.cfg.BaseURL, h.verifySigningKey, res.Email)
	body := fmt.Sprintf(
		"%s invited you to play %s.\n\n"+
			"Accept the invite and set up your player account here:\n%s\n\n"+
			"ggscale is the player-account service %s uses for sign-in, saves, friends, and leaderboards. "+
			"One ggscale account works across every game built on it.\n\n"+
			"This invite expires %s.\n\n"+
			"Don't want invite emails? Unsubscribe: %s",
		inviterEmail, gameName, link, gameName,
		res.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"), unsubscribe)
	if err := h.mailer.Send(ctx, mailer.Message{
		From:            h.cfg.MailFrom,
		To:              []string{res.Email},
		Subject:         "You're invited to play " + gameName,
		Body:            body,
		ListUnsubscribe: unsubscribe,
	}); err != nil {
		slog.ErrorContext(ctx, "player invite mailer", "err", err, "invite_id", res.ID)
	}
}

// projectDisplayName resolves the project (game) name for invite copy, with
// a generic fallback when the lookup fails or the name is not header-safe.
func (h *Handler) projectDisplayName(ctx context.Context, tenantID, projectID int64) string {
	const fallback = "a game on ggscale"
	projects, err := h.listProjects(ctx, tenantID)
	if err != nil {
		return fallback
	}
	for _, p := range projects {
		if p.ID == projectID {
			return headerSafeName(p.Name, fallback)
		}
	}
	return fallback
}

// PlayerView is one row in the player list/detail page.
type PlayerView struct {
	ID              int64
	ExternalID      string
	Email           string
	EmailVerifiedAt time.Time
	DisabledAt      time.Time
	CreatedAt       time.Time
	// DeleteRequestedAt and ScheduledPurgeAt are set while a data-deletion
	// request is pending; zero otherwise.
	DeleteRequestedAt time.Time
	ScheduledPurgeAt  time.Time
	// Account link + remote-address / ban fields. AccountID is empty for
	// anonymous players.
	AccountID    string
	RemoteAddrs  []RemoteAddrView
	TenantBanned bool
	// InvitePending is true when an open "link player" invitation targets this
	// row (awaiting the player's acceptance).
	InvitePending bool
}

// RemoteAddrView is one typed remote address on the player detail card.
type RemoteAddrView struct {
	TypeLabel  string
	ScopeLabel string
	Address    string
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
	offset := controlPanelPageOffset(page, playersPerPage)

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
			Lim:         controlPanelPageLimit(playersPerPage),
			Off:         offset,
		})
		if err != nil {
			return err
		}
		if len(rows) > playersPerPage {
			hasNext = true
			rows = rows[:playersPerPage]
		}
		targets, terr := q.ListOpenInvitationTargetsForProject(ctx, projectID)
		if terr != nil {
			return terr
		}
		pending := make(map[int64]bool, len(targets))
		for _, id := range targets {
			if id != nil {
				pending[*id] = true
			}
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
			if row.DeleteRequestedAt.Valid {
				pv.DeleteRequestedAt = row.DeleteRequestedAt.Time
				pv.ScheduledPurgeAt = row.DeleteRequestedAt.Time.Add(h.deleteGrace())
			}
			pv.InvitePending = pending[row.ID]
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
	pv := playerViewFromDetail(row)
	if !pv.DeleteRequestedAt.IsZero() {
		pv.ScheduledPurgeAt = pv.DeleteRequestedAt.Add(h.deleteGrace())
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, PlayerDetailPage(PlayerDetailView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		TenantID:  tenantID,
		ProjectID: projectID,
		Player:    pv,
		Message:   r.URL.Query().Get("flash"),
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
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectPlayersObject(projectID), rbac.ActionManage) {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	email := normalizeEmail(r.Form.Get("email"))
	if !validControlPanelEmail(email) {
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
	res, retry, err := h.createAndSendPlayerInvite(r, tenantID, projectID, session.User.ID, email, nil)
	if err != nil {
		view := InvitePlayerView{
			UserEmail: session.User.Email, CSRFToken: session.CSRFToken,
			TenantID: tenantID, ProjectID: projectID, Email: email,
		}
		switch {
		case errors.Is(err, errInviteThrottled):
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			view.Error = "Too many invites in a short time. Try again in " + strconv.Itoa(retry) + "s."
			w.WriteHeader(http.StatusTooManyRequests)
		case errors.Is(err, errProjectNotInTenant):
			view.Error = "That Game Project does not belong to this Account Tenant."
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
	target := pathTenantsPrefix + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) +
		"/players?flash=" + url.QueryEscape("Invite sent to "+res.Email)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// LinkPlayerView is the data rendered by the "link player" dialog.
type LinkPlayerView struct {
	CSRFToken  string
	TenantID   int64
	ProjectID  int64
	PlayerID   int64
	ExternalID string
	Email      string
	// Error re-renders the dialog with an inline banner when a submit fails, so
	// the admin keeps the form instead of navigating to a bare error page.
	Error string
}

// linkPlayerDialog renders the modal fragment (loaded via hx-get into
// #modal-root) with the player's read-only external ID and an email field.
func (h *Handler) linkPlayerDialog(w http.ResponseWriter, r *http.Request) {
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
			TenantID: tenantID, ProjectID: projectID, ID: playerID,
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
	pv := playerViewFromDetail(row)
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, LinkPlayerDialog(LinkPlayerView{
		CSRFToken:  session.CSRFToken,
		TenantID:   tenantID,
		ProjectID:  projectID,
		PlayerID:   playerID,
		ExternalID: pv.ExternalID,
		Email:      pv.Email, // prefill any existing (unverified) email
	}))
}

// linkPlayerHandler sends a "link player" invite targeting an existing row: on
// accept the proven email + account bind onto that row (see createPlayerInvite).
func (h *Handler) linkPlayerHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	projectID, ok := parsePathID(w, r, "projectID")
	if !ok {
		return
	}
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectPlayersObject(projectID), rbac.ActionManage) {
		return
	}
	playerID, ok := parsePathID(w, r, "playerID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	email := normalizeEmail(r.Form.Get("email"))
	// external_id round-trips as a hidden field so an error re-render keeps the
	// read-only ID without a second lookup; it is display-only, never trusted.
	vm := LinkPlayerView{
		CSRFToken:  session.CSRFToken,
		TenantID:   tenantID,
		ProjectID:  projectID,
		PlayerID:   playerID,
		ExternalID: r.Form.Get("external_id"),
		Email:      email,
	}
	if !validControlPanelEmail(email) {
		h.renderLinkDialogError(w, r, vm, http.StatusUnprocessableEntity, "Enter a valid email.")
		return
	}
	res, retry, err := h.createAndSendPlayerInvite(r, tenantID, projectID, session.User.ID, email, &playerID)
	if err != nil {
		switch {
		case errors.Is(err, errInviteThrottled):
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			h.renderLinkDialogError(w, r, vm, http.StatusTooManyRequests,
				"Too many invites in a short time. Try again in "+strconv.Itoa(retry)+"s.")
		case errors.Is(err, errProjectNotInTenant):
			h.renderLinkDialogError(w, r, vm, http.StatusNotFound, "That Game Project does not belong to this Account Tenant.")
		case errors.Is(err, errPlayerEmailTaken):
			h.renderLinkDialogError(w, r, vm, http.StatusConflict, "That email is already used by another player in this Game Project.")
		case isUniqueViolation(err):
			h.renderLinkDialogError(w, r, vm, http.StatusConflict, "An invite for that email is already pending.")
		default:
			slog.ErrorContext(r.Context(), "player link: create", "err", err)
			h.renderLinkDialogError(w, r, vm, http.StatusInternalServerError, "Invite could not be sent.")
		}
		return
	}
	htmxRedirect(w, r, pathTenantsPrefix+strconv.FormatInt(tenantID, 10)+
		"/projects/"+strconv.FormatInt(projectID, 10)+
		"/players?flash="+url.QueryEscape("Invite sent to "+res.Email))
}

// renderLinkDialogError re-renders the Link dialog fragment with an inline
// error banner and the given status. htmx swaps it back into #modal-root (the
// htmx-config permits swapping 409/422); a non-htmx client still gets the
// proper status code.
func (h *Handler) renderLinkDialogError(w http.ResponseWriter, r *http.Request, vm LinkPlayerView, status int, msg string) {
	vm.Error = msg
	w.WriteHeader(status)
	webutil.Render(r, w, LinkPlayerDialog(vm))
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
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectPlayersObject(projectID), rbac.ActionManage) {
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
	var updated int64
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		var terr error
		updated, terr = sqlcgen.New(tx).SetPlayerDisabledInProject(ctx, sqlcgen.SetPlayerDisabledInProjectParams{
			ID:         playerID,
			ProjectID:  projectID,
			TenantID:   tenantID,
			DisabledAt: disabledAt,
		})
		return terr
	})
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if updated == 0 {
		// The guarded UPDATE skips players with a pending deletion — that
		// state owns the disable flag until the deletion is cancelled.
		if h.playerHasPendingDelete(ctx, tenantID, projectID, playerID) {
			http.Error(w, "deletion pending; cancel the deletion first", http.StatusConflict)
			return
		}
		http.NotFound(w, r)
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

// defaultDeleteGracePeriod backs Config.DeleteGracePeriod when unset, keeping
// test fixtures and sparse deployments on the documented 30-day window.
const defaultDeleteGracePeriod = 720 * time.Hour

func (h *Handler) deleteGrace() time.Duration {
	if h.cfg.DeleteGracePeriod > 0 {
		return h.cfg.DeleteGracePeriod
	}
	return defaultDeleteGracePeriod
}

// playerHasPendingDelete reads the pending-deletion flag to pick the right
// error for a guarded write that matched no rows. A lookup failure reads as
// "no pending delete" — the caller then answers 404, which is still accurate
// for a vanished row.
func (h *Handler) playerHasPendingDelete(ctx context.Context, tenantID, projectID, playerID int64) bool {
	var pending bool
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).GetPlayerForProject(ctx, sqlcgen.GetPlayerForProjectParams{
			TenantID: tenantID, ProjectID: projectID, ID: playerID,
		})
		if qerr != nil {
			return qerr
		}
		pending = row.DeleteRequestedAt.Valid
		return nil
	})
	return err == nil && pending
}

func (h *Handler) playerRequestDeleteHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	projectID, ok := parsePathID(w, r, "projectID")
	if !ok {
		return
	}
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectPlayersObject(projectID), rbac.ActionManage) {
		return
	}
	playerID, ok := parsePathID(w, r, "playerID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	ctx := db.WithTenant(r.Context(), tenantID)
	var scheduledPurgeAt time.Time
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		requestedAt, qerr := q.RequestPlayerDeleteInProject(ctx, sqlcgen.RequestPlayerDeleteInProjectParams{
			ID:        playerID,
			ProjectID: projectID,
			TenantID:  tenantID,
		})
		if qerr != nil {
			return qerr
		}
		scheduledPurgeAt = requestedAt.Time.Add(h.deleteGrace())
		if _, qerr = q.RevokeActivePlayerSessions(ctx, sqlcgen.RevokeActivePlayerSessionsParams{
			ProjectID: projectID, PlayerID: playerID,
		}); qerr != nil {
			return qerr
		}
		return auditlog.WritePlatform(ctx, tx, session.User.ID, "control_panel.player.delete_request",
			strconv.FormatInt(playerID, 10), map[string]any{
				"tenant_id":          tenantID,
				"project_id":         projectID,
				"scheduled_purge_at": scheduledPurgeAt,
			})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		if h.playerHasPendingDelete(ctx, tenantID, projectID, playerID) {
			http.Error(w, "deletion already requested", http.StatusConflict)
			return
		}
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "delete request failed", http.StatusInternalServerError)
		return
	}
	target := pathTenantsPrefix + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) +
		"/players/" + strconv.FormatInt(playerID, 10) + queryFlash +
		url.QueryEscape("Deletion scheduled for "+scheduledPurgeAt.Format("2006-01-02")+". Cancel until then to keep the data.")
	htmxRedirect(w, r, target)
}

func (h *Handler) playerCancelDeleteHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	projectID, ok := parsePathID(w, r, "projectID")
	if !ok {
		return
	}
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectPlayersObject(projectID), rbac.ActionManage) {
		return
	}
	playerID, ok := parsePathID(w, r, "playerID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	ctx := db.WithTenant(r.Context(), tenantID)
	var cancelled int64
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var qerr error
		cancelled, qerr = q.CancelPlayerDeleteInProject(ctx, sqlcgen.CancelPlayerDeleteInProjectParams{
			ID:        playerID,
			ProjectID: projectID,
			TenantID:  tenantID,
		})
		if qerr != nil || cancelled == 0 {
			return qerr
		}
		return auditlog.WritePlatform(ctx, tx, session.User.ID, "control_panel.player.delete_cancel",
			strconv.FormatInt(playerID, 10), map[string]any{
				"tenant_id":  tenantID,
				"project_id": projectID,
			})
	})
	if err != nil {
		http.Error(w, "delete cancel failed", http.StatusInternalServerError)
		return
	}
	if cancelled == 0 {
		http.Error(w, "no pending deletion", http.StatusNotFound)
		return
	}
	target := pathTenantsPrefix + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) +
		"/players/" + strconv.FormatInt(playerID, 10) + queryFlash +
		url.QueryEscape("Deletion cancelled. The player's data is kept.")
	htmxRedirect(w, r, target)
}

func playerViewFromDetail(row sqlcgen.GetPlayerForProjectRow) PlayerView {
	pv := PlayerView{
		ID:                row.ID,
		ExternalID:        row.ExternalID,
		Email:             row.Email,
		EmailVerifiedAt:   row.EmailVerifiedAt.Time,
		DisabledAt:        row.DisabledAt.Time,
		CreatedAt:         row.CreatedAt.Time,
		DeleteRequestedAt: row.DeleteRequestedAt.Time,
		TenantBanned:      row.TenantBanned,
	}
	if row.PlayerAccountID.Valid {
		pv.AccountID = uuid.UUID(row.PlayerAccountID.Bytes).String()
	}
	set := remoteaddr.SetFromValues(row.RemoteAddrIpLan, row.RemoteAddrIpPublic, row.RemoteAddrDns, row.RemoteAddrIroh)
	for _, a := range set.List() {
		pv.RemoteAddrs = append(pv.RemoteAddrs, RemoteAddrView{
			TypeLabel:  remoteAddrTypeLabel(a),
			ScopeLabel: remoteAddrScopeLabel(a.Scope),
			Address:    a.Value,
		})
	}
	return pv
}

func remoteAddrTypeLabel(a remoteaddr.Address) string {
	switch a.Slot() {
	case remoteaddr.SlotIPLAN:
		return "LAN IP"
	case remoteaddr.SlotIPPublic:
		return "Public IP"
	case remoteaddr.SlotDNS:
		return "DNS name"
	default:
		return "Iroh endpoint"
	}
}

func remoteAddrScopeLabel(s remoteaddr.Scope) string {
	switch s {
	case remoteaddr.ScopeLAN:
		return "LAN"
	case remoteaddr.ScopePublic:
		return "public"
	default:
		return ""
	}
}

// playerToggleBanHandler bans / unbans a player's GLOBAL account across the
// tenant. Requires the player to be linked to an account. Bumps session_epoch
// on every player of the account in this tenant so live JWTs die immediately.
func (h *Handler) playerToggleBanHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	projectID, ok := parsePathID(w, r, "projectID")
	if !ok {
		return
	}
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectPlayersObject(projectID), rbac.ActionManage) {
		return
	}
	playerID, ok := parsePathID(w, r, "playerID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	ban := r.Form.Get("ban") == "true"
	reason := strings.TrimSpace(r.Form.Get("reason"))
	session, _ := sessionFromContext(r.Context())
	ctx := db.WithTenant(r.Context(), tenantID)

	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		row, err := q.GetPlayerForProject(ctx, sqlcgen.GetPlayerForProjectParams{
			TenantID: tenantID, ProjectID: projectID, ID: playerID,
		})
		if err != nil {
			return err
		}
		if !row.PlayerAccountID.Valid {
			return errPlayerNotLinked
		}
		acctID := row.PlayerAccountID
		if ban {
			var reasonPtr *string
			if reason != "" {
				reasonPtr = &reason
			}
			actor := session.User.ID
			if err := q.CreateTenantPlayerBan(ctx, sqlcgen.CreateTenantPlayerBanParams{
				TenantID: tenantID, PlayerAccountID: acctID, Reason: reasonPtr, CreatedBy: &actor,
			}); err != nil {
				return err
			}
		} else if _, err := q.DeleteTenantPlayerBan(ctx, sqlcgen.DeleteTenantPlayerBanParams{
			TenantID: tenantID, PlayerAccountID: acctID,
		}); err != nil {
			return err
		}
		if err := q.BumpAccountPlayerEpochsInTenant(ctx, sqlcgen.BumpAccountPlayerEpochsInTenantParams{
			TenantID: tenantID, PlayerAccountID: acctID,
		}); err != nil {
			return err
		}
		action := "control_panel.player.tenant_unban"
		if ban {
			action = "control_panel.player.tenant_ban"
		}
		return auditlog.WritePlatform(ctx, tx, session.User.ID, action,
			"player_account:"+uuid.UUID(acctID.Bytes).String(),
			map[string]any{"tenant_id": tenantID, "reason": reason})
	})
	if errors.Is(err, errPlayerNotLinked) {
		http.Error(w, "player has no linked gg-scale account to ban", http.StatusBadRequest)
		return
	}
	if err != nil {
		webutil.InternalError(w, "player ban toggle", err)
		return
	}
	if ban {
		h.metrics.BanIssued(observability.BanScopeTenant)
	}
	flash := "Player banned across the Account Tenant."
	if !ban {
		flash = "Player unbanned."
	}
	target := pathTenantsPrefix + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) +
		"/players/" + strconv.FormatInt(playerID, 10) + queryFlash + url.QueryEscape(flash)
	htmxRedirect(w, r, target)
}

var errPlayerNotLinked = errors.New("control panel: player has no linked account")
