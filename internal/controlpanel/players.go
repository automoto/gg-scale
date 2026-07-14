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

	"github.com/ggscale/ggscale/internal/auditlog"
	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/remoteaddr"
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
	h.sendPlayerInviteEmail(r.Context(), res, projectID)
	return res, 0, nil
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
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, PlayerDetailPage(PlayerDetailView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		TenantID:  tenantID,
		ProjectID: projectID,
		Player:    playerViewFromDetail(row),
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
			h.renderLinkDialogError(w, r, vm, http.StatusNotFound, "That project does not belong to this tenant.")
		case errors.Is(err, errPlayerEmailTaken):
			h.renderLinkDialogError(w, r, vm, http.StatusConflict, "That email is already used by another player in this project.")
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
		return sqlcgen.New(tx).SetPlayerDisabledInProject(ctx, sqlcgen.SetPlayerDisabledInProjectParams{
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

func playerViewFromDetail(row sqlcgen.GetPlayerForProjectRow) PlayerView {
	pv := PlayerView{
		ID:              row.ID,
		ExternalID:      row.ExternalID,
		Email:           row.Email,
		EmailVerifiedAt: row.EmailVerifiedAt.Time,
		DisabledAt:      row.DisabledAt.Time,
		CreatedAt:       row.CreatedAt.Time,
		TenantBanned:    row.TenantBanned,
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
	flash := "Player banned tenant-wide."
	if !ban {
		flash = "Player unbanned."
	}
	target := pathTenantsPrefix + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) +
		"/players/" + strconv.FormatInt(playerID, 10) + queryFlash + url.QueryEscape(flash)
	htmxRedirect(w, r, target)
}

var errPlayerNotLinked = errors.New("control panel: player has no linked account")
