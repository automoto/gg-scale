package dashboard

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ggscale/ggscale/internal/auditlog"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/webutil"
)

const platformUsersPerPage = 25

// errCannotDisableSelf is returned when a platform admin tries to
// disable their own account.
var errCannotDisableSelf = errors.New("dashboard: cannot disable yourself")
var errCannotDisableLastPlatformAdmin = errors.New("dashboard: cannot disable the last platform admin")

// validateUserDisableTarget rejects the actor disabling themselves. Pure
// helper so it can be unit-tested without HTTP plumbing.
func validateUserDisableTarget(actorID, targetID int64) error {
	if actorID == targetID {
		return errCannotDisableSelf
	}
	return nil
}

func (h *Handler) platformUsersPage(w http.ResponseWriter, r *http.Request) {
	search := r.URL.Query().Get("q")
	page := pageParam(r)
	offset := dashboardPageOffset(page, platformUsersPerPage)

	var (
		rows    []sqlcgen.ListDashboardUsersForPlatformAdminRow
		total   int64
		hasNext bool
	)
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var filter *string
		if search != "" {
			filter = &search
		}
		var err error
		rows, err = q.ListDashboardUsersForPlatformAdmin(r.Context(), sqlcgen.ListDashboardUsersForPlatformAdminParams{
			EmailFilter: filter,
			Lim:         dashboardPageLimit(platformUsersPerPage),
			Off:         offset,
		})
		if err != nil {
			return err
		}
		if len(rows) > platformUsersPerPage {
			hasNext = true
			rows = rows[:platformUsersPerPage]
		}
		total = int64(offset) + int64(len(rows))
		if hasNext {
			total++
		}
		return nil
	})
	if err != nil {
		webutil.InternalError(w, "platform users: list", err)
		return
	}

	session, _ := sessionFromContext(r.Context())
	users := make([]UserView, 0, len(rows))
	for _, row := range rows {
		users = append(users, UserView{
			ID:              row.ID,
			Email:           row.Email,
			IsPlatformAdmin: row.IsPlatformAdmin,
			DisabledAt:      row.DisabledAt.Time,
			LastLoginAt:     row.LastLoginAt.Time,
			CreatedAt:       row.CreatedAt.Time,
			TenantCount:     row.TenantCount,
			IsSelf:          row.ID == session.User.ID,
		})
	}
	webutil.Render(r, w, PlatformUsersPage(PlatformUsersView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		Search:    search,
		Users:     users,
		Total:     total,
		Page:      page,
		HasPrev:   page > 1,
		HasNext:   hasNext,
		Message:   r.URL.Query().Get("flash"),
	}))
}

func (h *Handler) disableDashboardUserHandler(w http.ResponseWriter, r *http.Request) {
	h.toggleDashboardUserDisabled(w, r, true)
}

func (h *Handler) enableDashboardUserHandler(w http.ResponseWriter, r *http.Request) {
	h.toggleDashboardUserDisabled(w, r, false)
}

func (h *Handler) toggleDashboardUserDisabled(w http.ResponseWriter, r *http.Request, disable bool) {
	userID, ok := parsePathID(w, r, "userID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	if disable {
		if err := validateUserDisableTarget(session.User.ID, userID); err != nil {
			http.Redirect(w, r,
				pathAdminUsersFlash+url.QueryEscape("You can't disable yourself; ask another platform admin."),
				http.StatusSeeOther)
			return
		}
	}

	var disabledAt pgtype.Timestamptz
	action := "dashboard.user.enabled"
	flashMsg := "User re-enabled."
	if disable {
		disabledAt = pgtype.Timestamptz{Time: h.now(), Valid: true}
		action = "dashboard.user.disabled"
		flashMsg = "User disabled. Sessions and pending invitations revoked."
	}

	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		target, err := q.GetDashboardUserByID(r.Context(), userID)
		if err != nil {
			return err
		}
		if disable && target.IsPlatformAdmin {
			if _, err := q.LockEnabledPlatformAdmins(r.Context()); err != nil {
				return err
			}
			admins, err := q.CountEnabledPlatformAdmins(r.Context())
			if err != nil {
				return err
			}
			if admins <= 1 {
				return errCannotDisableLastPlatformAdmin
			}
		}
		if err := q.SetDashboardUserDisabled(r.Context(), sqlcgen.SetDashboardUserDisabledParams{
			ID:         userID,
			DisabledAt: disabledAt,
		}); err != nil {
			return err
		}
		// Only revoke sessions + invitations on disable; on re-enable
		// the user has to log in again and the platform admin can
		// re-issue invitations if needed.
		if disable {
			if err := q.RevokeAllDashboardSessionsForUser(r.Context(), userID); err != nil {
				return err
			}
			if err := q.RevokeOpenInvitationsByInviter(r.Context(), userID); err != nil {
				return err
			}
		}
		return auditlog.WritePlatform(r.Context(), tx, session.User.ID, action,
			"user:"+strconv.FormatInt(userID, 10),
			map[string]any{
				"target_user_id": userID,
				"email":          target.Email,
			})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, errCannotDisableLastPlatformAdmin) {
		http.Redirect(w, r,
			pathAdminUsersFlash+url.QueryEscape("You can't disable the last enabled platform admin."),
			http.StatusSeeOther)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "dashboard user toggle", "err", err, "user_id", userID, "disable", disable)
		webutil.InternalError(w, "user toggle", err)
		return
	}

	htmxRedirect(w, r, pathAdminUsersFlash+url.QueryEscape(flashMsg))
}
