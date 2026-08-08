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
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/period"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/webutil"
)

var errDuplicateLeaderboard = errors.New("control panel: leaderboard with that name already exists")

// errSortOrderLocked rejects a sort-order change on a board that already has
// entries: collapsed bests are frozen under the order they were written with.
var errSortOrderLocked = errors.New("control panel: sort order is fixed once scores exist")

const (
	sortOrderAsc  = "asc"
	sortOrderDesc = "desc"

	scoreOperatorBest = "best"
	resetScheduleNone = period.ScheduleNone

	// maxLeaderboardMetadataBytes caps the per-board display blob. It is
	// deliberately smaller than remote config: every /v1/leaderboards reply
	// carries it for every board.
	maxLeaderboardMetadataBytes = 16 << 10
)

// leaderboardFormFields is the parsed create/edit form. ScoreOperator is
// empty on edit — the operator is fixed at creation.
type leaderboardFormFields struct {
	Name              string
	SortOrder         string
	ScoreOperator     string
	ClientSubmissions bool
	ScoreMin          *int64
	ScoreMax          *int64
	ResetSchedule     string
	AttemptCap        *int32
	Metadata          []byte
}

// leaderboardNameMax bounds a board name. The column is unbounded text, so
// without a limit the form accepts an arbitrarily long value.
const leaderboardNameMax = 120

// validLeaderboardName rejects names PostgreSQL cannot store or that are
// unbounded. A NUL byte raises SQLSTATE 22021, which the duplicate-name
// translator does not match, so it would render as a 500 instead of a field
// error. Invalid UTF-8 is checked first because the rune loop below would see
// it as U+FFFD.
func validLeaderboardName(name string) bool {
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > leaderboardNameMax {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// parseLeaderboardForm validates the shared create/edit form and collects
// per-field errors. Blank optional fields default (best operator, no
// schedule, no bounds, no cap, no metadata) so pre-feature forms keep
// working unchanged.
func parseLeaderboardForm(form url.Values, edit bool) (leaderboardFormFields, map[string]string) {
	errs := map[string]string{}
	fields := leaderboardFormFields{Name: strings.TrimSpace(form.Get("name"))}
	switch {
	case fields.Name == "":
		errs["name"] = "Name is required."
	case !validLeaderboardName(fields.Name):
		errs["name"] = fmt.Sprintf("Name must be 1–%d characters and cannot contain control characters.", leaderboardNameMax)
	}

	var sortOK bool
	fields.SortOrder, sortOK = normalizeSortOrder(form.Get("sort_order"))
	if !sortOK {
		fields.SortOrder = sortOrderDesc
		errs["sort_order"] = "Sort order must be ascending or descending."
	}

	if !edit {
		switch op := strings.ToLower(strings.TrimSpace(form.Get("score_operator"))); op {
		case "":
			fields.ScoreOperator = scoreOperatorBest
		case scoreOperatorBest, "set", "incr":
			fields.ScoreOperator = op
		default:
			fields.ScoreOperator = scoreOperatorBest
			errs["score_operator"] = "Score operator must be best, set, or incr."
		}
	}

	switch sched := strings.ToLower(strings.TrimSpace(form.Get("reset_schedule"))); {
	case sched == "":
		fields.ResetSchedule = resetScheduleNone
	case period.ValidSchedule(sched):
		fields.ResetSchedule = sched
	default:
		fields.ResetSchedule = resetScheduleNone
		errs["reset_schedule"] = "Reset schedule must be none, daily, weekly, or monthly."
	}

	fields.ClientSubmissions = form.Get("client_submissions") != ""
	fields.ScoreMin = parseOptionalScore(form.Get("score_min"), "score_min", "Minimum score", errs)
	fields.ScoreMax = parseOptionalScore(form.Get("score_max"), "score_max", "Maximum score", errs)
	if fields.ScoreMin != nil && fields.ScoreMax != nil && *fields.ScoreMin > *fields.ScoreMax {
		errs["score_min"] = "Minimum score must not exceed the maximum."
	}

	if raw := strings.TrimSpace(form.Get("attempt_cap")); raw != "" {
		cap64, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || cap64 <= 0 {
			errs["attempt_cap"] = "Attempt cap must be a positive whole number."
		} else {
			cap32 := int32(cap64)
			fields.AttemptCap = &cap32
		}
	}

	if raw := strings.TrimSpace(form.Get("metadata")); raw != "" {
		encoded, err := normalizeLeaderboardMetadata(raw)
		if err != nil {
			errs["metadata"] = "Metadata must be a JSON object no larger than 16 KiB."
		} else {
			fields.Metadata = encoded
		}
	}
	return fields, errs
}

func parseOptionalScore(raw, field, label string, errs map[string]string) *int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		errs[field] = label + " must be a whole number."
		return nil
	}
	return &v
}

// normalizeLeaderboardMetadata mirrors the remote-config rules at a smaller
// cap: a single top-level JSON object, re-encoded canonically.
func normalizeLeaderboardMetadata(raw string) ([]byte, error) {
	return normalizeJSONObjectBlob(raw, maxLeaderboardMetadataBytes)
}

// normalizeSortOrder trims and lowercases the submitted sort order, defaulting
// an empty value to "desc" (higher score ranks first). It reports ok=false for
// anything other than asc/desc so the handler surfaces a field error before the
// DB CHECK constraint would.
func normalizeSortOrder(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return sortOrderDesc, true
	case sortOrderAsc:
		return sortOrderAsc, true
	case sortOrderDesc:
		return sortOrderDesc, true
	default:
		return "", false
	}
}

// leaderboardsBasePath builds a project's leaderboard CRUD route prefix; it is
// shared by the handlers and the templates.
func leaderboardsBasePath(tenantID, projectID int64) string {
	return pathTenantsPrefix + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/leaderboards"
}

func (h *Handler) leaderboardsListPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	boards, err := h.listLeaderboards(r.Context(), tenantID, projectID)
	if err != nil {
		slog.ErrorContext(r.Context(), "leaderboards list failed", "err", err)
		http.Error(w, "leaderboards list failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, LeaderboardsListPage(LeaderboardsListView{
		UserEmail:    session.User.Email,
		CSRFToken:    session.CSRFToken,
		TenantID:     tenantID,
		ProjectID:    projectID,
		Leaderboards: boards,
		Message:      r.URL.Query().Get("flash"),
	}))
}

func (h *Handler) leaderboardsNewPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, NewLeaderboardPage(LeaderboardFormView{
		UserEmail:     session.User.Email,
		CSRFToken:     session.CSRFToken,
		TenantID:      tenantID,
		ProjectID:     projectID,
		SortOrder:     sortOrderDesc,
		ScoreOperator: scoreOperatorBest,
		ResetSchedule: resetScheduleNone,
	}))
}

func (h *Handler) leaderboardsCreateHandler(w http.ResponseWriter, r *http.Request) {
	h.leaderboardFormHandler(w, r, false)
}

func (h *Handler) leaderboardsUpdateHandler(w http.ResponseWriter, r *http.Request) {
	h.leaderboardFormHandler(w, r, true)
}

// leaderboardFormHandler is the shared create/update form flow: parse,
// validate, check permission, save, audit, redirect with a flash. With
// edit=true it reads the leaderboard id from the path and updates; otherwise
// it creates.
func (h *Handler) leaderboardFormHandler(w http.ResponseWriter, r *http.Request, edit bool) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	var id int64
	if edit {
		if id, ok = parsePathID(w, r, "leaderboardID"); !ok {
			return
		}
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	page, action, verb := NewLeaderboardPage, "leaderboard.create", "created"
	if edit {
		page, action, verb = EditLeaderboardPage, "leaderboard.update", "updated"
	}
	session, _ := sessionFromContext(r.Context())
	fields, fieldErrs := parseLeaderboardForm(r.Form, edit)
	view := LeaderboardFormView{
		UserEmail:         session.User.Email,
		CSRFToken:         session.CSRFToken,
		TenantID:          tenantID,
		ProjectID:         projectID,
		LeaderboardID:     id,
		Name:              fields.Name,
		SortOrder:         fields.SortOrder,
		ScoreOperator:     fields.ScoreOperator,
		ClientSubmissions: fields.ClientSubmissions,
		ScoreMin:          strings.TrimSpace(r.Form.Get("score_min")),
		ScoreMax:          strings.TrimSpace(r.Form.Get("score_max")),
		ResetSchedule:     fields.ResetSchedule,
		AttemptCap:        strings.TrimSpace(r.Form.Get("attempt_cap")),
		Metadata:          strings.TrimSpace(r.Form.Get("metadata")),
		FieldErrors:       fieldErrs,
	}
	if edit {
		// The operator is not on the edit form; re-renders show the stored
		// one, and a 404 here beats one after validation.
		cur, err := h.getLeaderboard(r.Context(), tenantID, projectID, id)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			slog.ErrorContext(r.Context(), "leaderboard lookup failed", "err", err)
			http.Error(w, "leaderboard lookup failed", http.StatusInternalServerError)
			return
		}
		view.ScoreOperator = cur.ScoreOperator
		view.CurrentPeriod = cur.CurrentPeriod
	}
	if len(view.FieldErrors) > 0 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, page(view))
		return
	}
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectLeaderboardObject(projectID), rbac.ActionManage) {
		return
	}
	var err error
	if edit {
		err = h.updateLeaderboard(r.Context(), tenantID, projectID, id, fields)
	} else {
		id, err = h.createLeaderboard(r.Context(), tenantID, projectID, fields)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Deleted concurrently — nothing was written, so no success flash
		// and no audit row.
		http.NotFound(w, r)
		return
	case errors.Is(err, errDuplicateLeaderboard):
		view.FieldErrors["name"] = "A leaderboard with that name already exists."
		w.WriteHeader(http.StatusConflict)
		webutil.Render(r, w, page(view))
		return
	case errors.Is(err, errSortOrderLocked):
		view.FieldErrors["sort_order"] = "Sort order is fixed once scores exist."
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, page(view))
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "save leaderboard failed", "action", action, "err", err)
		view.Error = "Save failed."
		w.WriteHeader(http.StatusInternalServerError)
		webutil.Render(r, w, page(view))
		return
	}
	payload := map[string]any{
		"project_id":         projectID,
		"leaderboard_name":   fields.Name,
		"reset_schedule":     fields.ResetSchedule,
		"client_submissions": fields.ClientSubmissions,
	}
	if fields.AttemptCap != nil {
		payload["attempt_cap"] = *fields.AttemptCap
	}
	if !edit {
		payload["score_operator"] = fields.ScoreOperator
	}
	h.auditLeaderboard(r.Context(), tenantID, session.User.ID, action, id, payload)
	http.Redirect(w, r, leaderboardsBasePath(tenantID, projectID)+queryFlash+url.QueryEscape("Leaderboard \""+fields.Name+"\" "+verb+"."), http.StatusSeeOther)
}

func (h *Handler) leaderboardsEditPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	id, ok := parsePathID(w, r, "leaderboardID")
	if !ok {
		return
	}
	row, err := h.getLeaderboard(r.Context(), tenantID, projectID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "leaderboard lookup failed", "err", err)
		http.Error(w, "leaderboard lookup failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, EditLeaderboardPage(LeaderboardFormView{
		UserEmail:         session.User.Email,
		CSRFToken:         session.CSRFToken,
		TenantID:          tenantID,
		ProjectID:         projectID,
		LeaderboardID:     row.ID,
		Name:              row.Name,
		SortOrder:         row.SortOrder,
		ScoreOperator:     row.ScoreOperator,
		ClientSubmissions: row.ClientSubmissions,
		ScoreMin:          optionalInt64String(row.ScoreMin),
		ScoreMax:          optionalInt64String(row.ScoreMax),
		ResetSchedule:     row.ResetSchedule,
		AttemptCap:        optionalInt32String(row.AttemptCap),
		Metadata:          formatLeaderboardMetadata(row.Metadata),
		CurrentPeriod:     row.CurrentPeriod,
	}))
}

func optionalInt64String(v *int64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(*v, 10)
}

func optionalInt32String(v *int32) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(int64(*v), 10)
}

// formatLeaderboardMetadata pretty-prints the stored blob for the textarea;
// an empty blob renders as an empty field.
func formatLeaderboardMetadata(blob []byte) string {
	if len(blob) == 0 {
		return ""
	}
	return formatRemoteConfig(blob)
}

func (h *Handler) leaderboardsDeleteHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	id, ok := parsePathID(w, r, "leaderboardID")
	if !ok {
		return
	}
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectLeaderboardObject(projectID), rbac.ActionManage) {
		return
	}
	err := h.softDeleteLeaderboard(r.Context(), tenantID, projectID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "delete leaderboard failed", "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	h.auditLeaderboard(r.Context(), tenantID, session.User.ID, "leaderboard.delete", id, map[string]any{
		"project_id": projectID,
	})
	http.Redirect(w, r, leaderboardsBasePath(tenantID, projectID)+queryFlash+url.QueryEscape("Leaderboard deleted."), http.StatusSeeOther)
}

// ── store helpers (inline sqlc, tenant-scoped) ──────────────────────────────

func (h *Handler) listLeaderboards(ctx context.Context, tenantID, projectID int64) ([]LeaderboardRowView, error) {
	if h.pool == nil {
		return nil, errors.New(msgControlPanelPoolNeeded)
	}
	var out []LeaderboardRowView
	ctx = db.WithTenant(ctx, tenantID)
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		rows, err := sqlcgen.New(tx).ListLeaderboardsForProject(ctx, projectID)
		if err != nil {
			return fmt.Errorf("list leaderboards: %w", err)
		}
		out = make([]LeaderboardRowView, 0, len(rows))
		for _, row := range rows {
			v := LeaderboardRowView{ID: row.ID, Name: row.Name, SortOrder: row.SortOrder}
			if row.CreatedAt.Valid {
				v.CreatedAt = row.CreatedAt.Time
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

func (h *Handler) getLeaderboard(ctx context.Context, tenantID, projectID, id int64) (sqlcgen.GetLeaderboardForControlPanelRow, error) {
	var row sqlcgen.GetLeaderboardForControlPanelRow
	if h.pool == nil {
		return row, errors.New(msgControlPanelPoolNeeded)
	}
	ctx = db.WithTenant(ctx, tenantID)
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).GetLeaderboardForControlPanel(ctx, sqlcgen.GetLeaderboardForControlPanelParams{
			ProjectID: projectID,
			ID:        id,
		})
		return err
	})
	return row, err
}

func (h *Handler) createLeaderboard(ctx context.Context, tenantID, projectID int64, f leaderboardFormFields) (int64, error) {
	if h.pool == nil {
		return 0, errors.New(msgControlPanelPoolNeeded)
	}
	params := sqlcgen.CreateLeaderboardParams{
		ProjectID:         projectID,
		Name:              f.Name,
		SortOrder:         f.SortOrder,
		ScoreOperator:     f.ScoreOperator,
		Metadata:          f.Metadata,
		ClientSubmissions: f.ClientSubmissions,
		ScoreMin:          f.ScoreMin,
		ScoreMax:          f.ScoreMax,
		ResetSchedule:     f.ResetSchedule,
		AttemptCap:        f.AttemptCap,
	}
	// One clock for both fields: a second Now() straddling a calendar
	// boundary would put next_reset_at before period_started_at.
	now := time.Now().UTC()
	if next, ok := period.NextReset(f.ResetSchedule, now); ok {
		params.PeriodStartedAt = pgtype.Timestamptz{Time: now, Valid: true}
		params.NextResetAt = pgtype.Timestamptz{Time: next, Valid: true}
	}
	var id int64
	ctx = db.WithTenant(ctx, tenantID)
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		var err error
		id, err = sqlcgen.New(tx).CreateLeaderboard(ctx, params)
		return translateLeaderboardDuplicate(err)
	})
	return id, err
}

// updateLeaderboard saves the edit form onto a live leaderboard. It returns
// pgx.ErrNoRows when the row no longer exists (e.g. deleted concurrently), so
// callers never report success for a mutation that matched nothing. Period
// bookkeeping only moves when the schedule actually changes — recomputing
// next_reset_at on an unrelated edit would erase an overdue reset the job has
// not caught up with yet.
func (h *Handler) updateLeaderboard(ctx context.Context, tenantID, projectID, id int64, f leaderboardFormFields) error {
	if h.pool == nil {
		return errors.New(msgControlPanelPoolNeeded)
	}
	ctx = db.WithTenant(ctx, tenantID)
	return h.pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		cur, err := q.GetLeaderboardForControlPanel(ctx, sqlcgen.GetLeaderboardForControlPanelParams{
			ProjectID: projectID, ID: id,
		})
		if err != nil {
			return err
		}
		// The collapsed entry model freezes each best under the sort order
		// it was written with, so the direction is only editable while the
		// board has no entries in any period. (A submit racing this check is
		// a milliseconds-wide window; at worst one entry lands under the old
		// order, the same exposure as two adjacent submits.)
		if f.SortOrder != cur.SortOrder {
			hasEntries, herr := q.LeaderboardHasEntries(ctx, id)
			if herr != nil {
				return herr
			}
			if hasEntries {
				return errSortOrderLocked
			}
		}
		periodStarted, nextReset := cur.PeriodStartedAt, cur.NextResetAt
		if f.ResetSchedule != cur.ResetSchedule {
			now := time.Now().UTC()
			if next, ok := period.NextReset(f.ResetSchedule, now); ok {
				nextReset = pgtype.Timestamptz{Time: next, Valid: true}
				if !periodStarted.Valid || cur.ResetSchedule == resetScheduleNone {
					periodStarted = pgtype.Timestamptz{Time: now, Valid: true}
				}
			} else {
				// Schedule turned off: the current period persists, it just
				// stops resetting.
				nextReset = pgtype.Timestamptz{}
			}
		}
		n, err := q.UpdateLeaderboard(ctx, sqlcgen.UpdateLeaderboardParams{
			Name:              f.Name,
			SortOrder:         f.SortOrder,
			Metadata:          f.Metadata,
			ClientSubmissions: f.ClientSubmissions,
			ScoreMin:          f.ScoreMin,
			ScoreMax:          f.ScoreMax,
			ResetSchedule:     f.ResetSchedule,
			AttemptCap:        f.AttemptCap,
			PeriodStartedAt:   periodStarted,
			NextResetAt:       nextReset,
			ProjectID:         projectID,
			ID:                id,
		})
		if err != nil {
			return translateLeaderboardDuplicate(err)
		}
		if n == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
}

// softDeleteLeaderboard hides a leaderboard; like updateLeaderboard it returns
// pgx.ErrNoRows when nothing matched.
func (h *Handler) softDeleteLeaderboard(ctx context.Context, tenantID, projectID, id int64) error {
	if h.pool == nil {
		return errors.New(msgControlPanelPoolNeeded)
	}
	ctx = db.WithTenant(ctx, tenantID)
	return h.pool.Q(ctx, func(tx pgx.Tx) error {
		n, err := sqlcgen.New(tx).SoftDeleteLeaderboard(ctx, sqlcgen.SoftDeleteLeaderboardParams{
			ProjectID: projectID,
			ID:        id,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
}

func translateLeaderboardDuplicate(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return errDuplicateLeaderboard
	}
	return err
}

// auditLeaderboard records a leaderboard mutation by a control panel user in
// platform_audit_log (the actor is a control_panel_user, not a player). Audit
// failure is logged, never fatal to the request.
func (h *Handler) auditLeaderboard(ctx context.Context, tenantID, actorUserID int64, action string, id int64, payload map[string]any) {
	if err := h.writePlatformAudit(ctx, tenantID, actorUserID, action, strconv.FormatInt(id, 10), payload); err != nil {
		slog.WarnContext(ctx, "audit log: "+action, "err", err)
	}
}
