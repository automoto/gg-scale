package controlpanel

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/auditlog"
	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/observability"
	"github.com/automoto/gg-scale/internal/projectadmin"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/remoteaddr"
	"github.com/automoto/gg-scale/internal/webutil"
)

const playersPerPage = 25

// errProjectNotInTenant is returned when the URL projectID does not
// belong to the URL tenantID — guards against tenant-A admins crafting
// requests against tenant-B projects.
var errProjectNotInTenant = errors.New("control panel: project not in tenant")

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
	// DeleteRequestedByAdmin is false while the player owns the request and can
	// still cancel it. Only the player detail page sets it.
	DeleteRequestedByAdmin bool
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
	res, err := h.playerInvites.Invite(r.Context(), tenantID, projectID, email, nil, h.now(), projectadmin.Actor{UserID: session.User.ID})
	if err != nil {
		view := InvitePlayerView{
			UserEmail: session.User.Email, CSRFToken: session.CSRFToken,
			TenantID: tenantID, ProjectID: projectID, Email: email,
		}
		var throttled *projectadmin.InviteThrottledError
		switch {
		case errors.As(err, &throttled):
			w.Header().Set("Retry-After", strconv.Itoa(throttled.RetryAfter))
			view.Error = "Too many invites in a short time. Try again in " + strconv.Itoa(throttled.RetryAfter) + "s."
			w.WriteHeader(http.StatusTooManyRequests)
		case errors.Is(err, projectadmin.ErrProjectNotInTenant):
			view.Error = "That Game Project does not belong to this Account Tenant."
			w.WriteHeader(http.StatusNotFound)
		case errors.Is(err, projectadmin.ErrInvitePending):
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
	res, err := h.playerInvites.Invite(r.Context(), tenantID, projectID, email, &playerID, h.now(), projectadmin.Actor{UserID: session.User.ID})
	if err != nil {
		var throttled *projectadmin.InviteThrottledError
		switch {
		case errors.As(err, &throttled):
			w.Header().Set("Retry-After", strconv.Itoa(throttled.RetryAfter))
			h.renderLinkDialogError(w, r, vm, http.StatusTooManyRequests,
				"Too many invites in a short time. Try again in "+strconv.Itoa(throttled.RetryAfter)+"s.")
		case errors.Is(err, projectadmin.ErrProjectNotInTenant), errors.Is(err, projectadmin.ErrPlayerNotFound):
			h.renderLinkDialogError(w, r, vm, http.StatusNotFound, "That player does not belong to this Game Project.")
		case errors.Is(err, projectadmin.ErrPlayerEmailTaken):
			h.renderLinkDialogError(w, r, vm, http.StatusConflict, "That email is already used by another player in this Game Project.")
		case errors.Is(err, projectadmin.ErrInvitePending):
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
	// Either direction changes state, so a missing or unknown value is
	// refused rather than read as one of them.
	enable, err := strconv.ParseBool(r.Form.Get("enable"))
	if err != nil {
		http.Error(w, "enable must be true or false", http.StatusBadRequest)
		return
	}
	session, _ := sessionFromContext(r.Context())
	err = projectadmin.SetPlayerDisabled(r.Context(), h.pool, tenantID, projectID, playerID, !enable, h.now(), projectadmin.Actor{UserID: session.User.ID})
	if errors.Is(err, projectadmin.ErrPlayerDeletePending) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if errors.Is(err, projectadmin.ErrPlayerNotFound) {
		http.NotFound(w, r)
		return
	}
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

func (h *Handler) deleteGrace() time.Duration {
	return projectadmin.DeleteGrace(h.cfg.DeleteGracePeriod)
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
	scheduledPurgeAt, err := projectadmin.RequestPlayerDelete(r.Context(), h.pool, tenantID, projectID, playerID, h.deleteGrace(), projectadmin.Actor{UserID: session.User.ID})
	if errors.Is(err, projectadmin.ErrPlayerDeleteRequested) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if errors.Is(err, projectadmin.ErrPlayerNotFound) {
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
		url.QueryEscape("Deletion scheduled for "+timeString(scheduledPurgeAt)+". Cancel until then to keep the data.")
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
	err := projectadmin.CancelPlayerDelete(r.Context(), h.pool, tenantID, projectID, playerID, projectadmin.Actor{UserID: session.User.ID})
	if errors.Is(err, projectadmin.ErrNoPendingDelete) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "delete cancel failed", http.StatusInternalServerError)
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
		ID:                     row.ID,
		ExternalID:             row.ExternalID,
		Email:                  row.Email,
		EmailVerifiedAt:        row.EmailVerifiedAt.Time,
		DisabledAt:             row.DisabledAt.Time,
		CreatedAt:              row.CreatedAt.Time,
		DeleteRequestedAt:      row.DeleteRequestedAt.Time,
		DeleteRequestedByAdmin: row.DeleteRequestedByAdmin,
		TenantBanned:           row.TenantBanned,
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
	ban, err := strconv.ParseBool(r.Form.Get("ban"))
	if err != nil {
		http.Error(w, "ban must be true or false", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(r.Form.Get("reason"))
	session, _ := sessionFromContext(r.Context())
	ctx := db.WithTenant(r.Context(), tenantID)

	err = h.pool.Q(ctx, func(tx pgx.Tx) error {
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
