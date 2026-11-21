package controlpanel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/automoto/gg-scale/internal/auditlog"
	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/managementtoken"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/webutil"
)

const (
	managementTokenDefaultDays   = 30
	managementTokenExpiryWarning = 7 * 24 * time.Hour
	managementTokenLabelMax      = 64

	accessNone  = "none"
	accessRead  = "read"
	accessWrite = "write"

	fillReadOnly  = "read_only"
	fillReadWrite = "read_write"

	reasonRole   = "Your role does not allow this."
	reasonGrant  = "Not enabled for this Account Tenant."
	reasonSwitch = "Turned off on this server."
)

var errManagementTokenLimit = errors.New("control panel: management token limit reached")

// ManagementTokensView renders a project's management token page.
type ManagementTokensView struct {
	UserEmail   string
	CSRFToken   string
	TenantID    int64
	ProjectID   int64
	Tokens      []ManagementTokenRowView
	Access      []AccessRowView
	Risky       []RiskyBoxView
	MaxDays     int
	DefaultDays int
	// NewToken is the token value, shown once right after creation, with
	// its access in words.
	NewToken       string
	NewTokenAccess string
	Message        string
	Error          string
	FieldErrors    map[string]string
	Label          string
	Days           string
}

// ManagementTokenRowView is one token in the list. Token values are never
// stored.
type ManagementTokenRowView struct {
	ID     int64
	Label  string
	Hint   string
	Access string
	Risky  bool
	// LikeURL fills the form with the same access.
	LikeURL      string
	CreatorEmail string
	ExpiresAt    time.Time
	ExpiresSoon  bool
	Expired      bool
	LastUsedAt   *time.Time
	// Inactive means the creator no longer passes (project, manage), so
	// each request with the token is refused.
	Inactive bool
	// LostScopes counts the scopes whose pair the creator no longer passes.
	LostScopes int
}

// AccessRowView is one row of the access table in the new token form.
type AccessRowView struct {
	Name     string
	Label    string
	HasWrite bool
	CanRead  bool
	CanWrite bool
	// Reason says why an option of the row is disabled.
	Reason string
	// Selected is the checked option: none, read, or write.
	Selected string

	read   string
	writes []string
}

// RiskyBoxView is one opt-in scope in the risky group of the form.
type RiskyBoxView struct {
	Scope string
	Label string
	// Reason says why the box is disabled.
	Reason  string
	Checked bool
}

func managementTokensPath(tenantID, projectID int64) string {
	return pathTenantsPrefix + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/management-tokens"
}

func (h *Handler) requireManagementFeature(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.cfg.ManagementAPIEnabled {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) managementMaxDays() int {
	if h.cfg.ManagementMaxExpiryDays > 0 {
		return h.cfg.ManagementMaxExpiryDays
	}
	return 365
}

func (h *Handler) managementMaxTokens() int64 {
	if h.cfg.ManagementMaxProjectTokens > 0 {
		return int64(h.cfg.ManagementMaxProjectTokens)
	}
	return 20
}

// grantableManagementScopes returns the scopes whose Casbin pair the user
// passes now. A token can never get a scope its creator could not use.
func (h *Handler) grantableManagementScopes(userID, tenantID, projectID int64) ([]string, error) {
	var out []string
	for _, s := range managementtoken.Scopes {
		ok, err := h.rbac.CanControlPanel(userID, tenantID, s.Pair(projectID), s.Action)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, s.Scope)
		}
	}
	return out, nil
}

// featureGate returns why a row that needs feature is disabled, or "".
func (h *Handler) featureGate(ctx context.Context, tenantID, projectID int64, feature rbac.Feature) (string, error) {
	if feature == "" {
		return "", nil
	}
	if !h.featureEnabledByEnv(string(feature)) {
		return reasonSwitch, nil
	}
	ok, err := h.rbac.FeatureEnabled(ctx, tenantID, projectID, feature)
	if err != nil || ok {
		return "", err
	}
	return reasonGrant, nil
}

// managementAccess returns the access table and the risky group with the
// options that the user can give now.
func (h *Handler) managementAccess(ctx context.Context, userID, tenantID, projectID int64) ([]AccessRowView, []RiskyBoxView, error) {
	grantable, err := h.grantableManagementScopes(userID, tenantID, projectID)
	if err != nil {
		return nil, nil, err
	}
	gates := map[string]string{}
	for _, res := range managementtoken.Resources {
		gates[res.Name], err = h.featureGate(ctx, tenantID, projectID, res.Feature)
		if err != nil {
			return nil, nil, err
		}
	}
	rows, risky := accessOptions(grantable, gates)
	return rows, risky, nil
}

// accessOptions builds the form options from the grantable scopes and the
// gate reason of each row.
func accessOptions(grantable []string, gates map[string]string) ([]AccessRowView, []RiskyBoxView) {
	var rows []AccessRowView
	for _, res := range managementtoken.Resources {
		writes := res.WriteScopes()
		row := AccessRowView{Name: res.Name, Label: res.Label, HasWrite: len(writes) > 0, read: res.Read, Reason: gates[res.Name]}
		if row.Reason != "" {
			rows = append(rows, row)
			continue
		}
		row.CanRead = slices.Contains(grantable, res.Read)
		for _, s := range writes {
			if slices.Contains(grantable, s) {
				row.writes = append(row.writes, s)
			}
		}
		row.CanWrite = row.CanRead && len(row.writes) > 0
		if !row.CanRead || row.HasWrite && !row.CanWrite {
			row.Reason = reasonRole
		}
		rows = append(rows, row)
	}
	var risky []RiskyBoxView
	for _, s := range managementtoken.Scopes {
		if s.Class != managementtoken.ClassRisky {
			continue
		}
		box := RiskyBoxView{Scope: s.Scope, Label: s.Scope, Reason: gates[s.Resource]}
		if box.Reason == "" && !slices.Contains(grantable, s.Scope) {
			box.Reason = reasonRole
		}
		risky = append(risky, box)
	}
	return rows, risky
}

// scopesFromAccessForm turns the access_<resource> and risky form fields
// into scopes. It refuses any option that the form shows disabled or does
// not have. An absent row field gives no access.
func scopesFromAccessForm(form url.Values, rows []AccessRowView, risky []RiskyBoxView) ([]string, error) {
	scopes := []string{}
	for _, row := range rows {
		switch v := form.Get("access_" + row.Name); {
		case v == "" || v == accessNone:
		case v == accessRead && row.CanRead:
			scopes = append(scopes, row.read)
		case v == accessWrite && row.CanWrite:
			scopes = append(scopes, row.read)
			scopes = append(scopes, row.writes...)
		default:
			return nil, fmt.Errorf("you cannot give %s access to %s", strconv.Quote(v), row.Label)
		}
	}
	for _, v := range form["risky"] {
		i := slices.IndexFunc(risky, func(b RiskyBoxView) bool { return b.Scope == v })
		if i < 0 || risky[i].Reason != "" {
			return nil, fmt.Errorf("you cannot give the scope %s", strconv.Quote(v))
		}
		if !slices.Contains(scopes, v) {
			scopes = append(scopes, v)
		}
	}
	return scopes, nil
}

// fillAccess selects the form options from q: a Quick fill, or the
// access_<resource> and risky values of a "New token like this" link or of
// a refused form. "Read" is the default. A disabled option steps down to
// the next one that the user can give. Quick fill never checks a risky box.
func fillAccess(q url.Values, rows []AccessRowView, risky []RiskyBoxView) {
	fill := q.Get("fill")
	for i := range rows {
		want := q.Get("access_" + rows[i].Name)
		switch fill {
		case fillReadOnly:
			want = accessRead
		case fillReadWrite:
			want = accessWrite
		}
		switch {
		case want == accessWrite && rows[i].CanWrite:
			rows[i].Selected = accessWrite
		case want != accessNone && rows[i].CanRead:
			rows[i].Selected = accessRead
		default:
			rows[i].Selected = accessNone
		}
	}
	if fill != "" {
		return
	}
	for i := range risky {
		risky[i].Checked = risky[i].Reason == "" && slices.Contains(q["risky"], risky[i].Scope)
	}
}

// tokenAccessLevel returns the option of res that scopes match: write only
// when they hold the read scope and every write scope of the row.
func tokenAccessLevel(scopes []string, res managementtoken.Resource) string {
	if !slices.Contains(scopes, res.Read) {
		return accessNone
	}
	writes := res.WriteScopes()
	if len(writes) > 0 && !slices.ContainsFunc(writes, func(s string) bool { return !slices.Contains(scopes, s) }) {
		return accessWrite
	}
	return accessRead
}

// tokenAccessValues returns the form values that give the same access as
// scopes, or less for a row that holds only some of its write scopes.
func tokenAccessValues(scopes []string) url.Values {
	v := url.Values{}
	for _, res := range managementtoken.Resources {
		v.Set("access_"+res.Name, tokenAccessLevel(scopes, res))
	}
	for _, s := range scopes {
		if sp, ok := managementtoken.Scope(s); ok && sp.Class == managementtoken.ClassRisky {
			v.Add("risky", s)
		}
	}
	return v
}

// accessWords describes scopes for people, and reports whether they hold
// risky access. A write scope that the row option does not cover is named.
func accessWords(scopes []string) (string, bool) {
	var parts, risky []string
	for _, res := range managementtoken.Resources {
		level := tokenAccessLevel(scopes, res)
		var words []string
		switch level {
		case accessWrite:
			words = []string{"read and write"}
		case accessRead:
			words = []string{"read"}
		}
		if level != accessWrite {
			for _, s := range res.WriteScopes() {
				if slices.Contains(scopes, s) {
					words = append(words, s)
				}
			}
		}
		if len(words) > 0 {
			parts = append(parts, res.Label+": "+strings.Join(words, ", "))
		}
	}
	for _, s := range scopes {
		if sp, ok := managementtoken.Scope(s); ok && sp.Class == managementtoken.ClassRisky {
			risky = append(risky, s)
		}
	}
	if len(risky) > 0 {
		parts = append(parts, "Risky: "+strings.Join(risky, ", "))
	}
	if len(parts) == 0 {
		return "No access", false
	}
	return strings.Join(parts, " · "), len(risky) > 0
}

func lostScopesText(n int) string {
	if n == 1 {
		return "1 permission inactive: creator lost access"
	}
	return strconv.Itoa(n) + " permissions inactive: creator lost access"
}

// lostScopes counts the scopes whose pair the creator fails now, with the
// same check as each request. A scope that is not in the table is skipped.
func (h *Handler) lostScopes(creatorID, tenantID, projectID int64, scopes []string) (int, error) {
	n := 0
	for _, name := range scopes {
		sp, ok := managementtoken.Scope(name)
		if !ok {
			continue
		}
		allowed, err := h.rbac.CanControlPanel(creatorID, tenantID, sp.Pair(projectID), sp.Action)
		if err != nil {
			return 0, err
		}
		if !allowed {
			n++
		}
	}
	return n, nil
}

func (h *Handler) managementTokensPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	view, err := h.managementTokensView(r.Context(), tenantID, projectID, r.URL.Query())
	if err != nil {
		webutil.InternalError(w, "management tokens: load", err)
		return
	}
	view.Message = r.URL.Query().Get("flash")
	webutil.Render(r, w, ManagementTokensPage(view))
}

// managementTokensView loads the page. fill selects the form options, as
// fillAccess reads them, and gives the label.
func (h *Handler) managementTokensView(ctx context.Context, tenantID, projectID int64, fill url.Values) (ManagementTokensView, error) {
	session, _ := sessionFromContext(ctx)
	view := ManagementTokensView{
		UserEmail:   session.User.Email,
		CSRFToken:   session.CSRFToken,
		TenantID:    tenantID,
		ProjectID:   projectID,
		MaxDays:     h.managementMaxDays(),
		DefaultDays: managementTokenDefaultDays,
		Days:        strconv.Itoa(managementTokenDefaultDays),
		Label:       fill.Get("label"),
	}
	var err error
	view.Access, view.Risky, err = h.managementAccess(ctx, session.User.ID, tenantID, projectID)
	if err != nil {
		return view, err
	}
	fillAccess(fill, view.Access, view.Risky)
	var rows []sqlcgen.ListManagementTokensForProjectRow
	tctx := db.WithTenant(ctx, tenantID)
	err = h.pool.Q(tctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlcgen.New(tx).ListManagementTokensForProject(tctx, projectID)
		return err
	})
	if err != nil {
		return view, err
	}
	now := time.Now()
	for _, row := range rows {
		active, err := h.rbac.CanControlPanel(row.CreatedByUserID, tenantID, rbac.ObjectProject, rbac.ActionManage)
		if err != nil {
			return view, err
		}
		lost, err := h.lostScopes(row.CreatedByUserID, tenantID, projectID, row.Scopes)
		if err != nil {
			return view, err
		}
		like := tokenAccessValues(row.Scopes)
		like.Set("label", row.Label)
		v := ManagementTokenRowView{
			ID:           row.ID,
			Label:        row.Label,
			Hint:         row.TokenHint,
			LikeURL:      managementTokensPath(tenantID, projectID) + "?" + like.Encode(),
			CreatorEmail: row.CreatorEmail,
			ExpiresAt:    row.ExpiresAt.Time,
			Expired:      !row.ExpiresAt.Time.After(now),
			ExpiresSoon:  row.ExpiresAt.Time.Sub(now) <= managementTokenExpiryWarning,
			Inactive:     !active,
			LostScopes:   lost,
		}
		v.Access, v.Risky = accessWords(row.Scopes)
		if row.LastUsedAt.Valid {
			v.LastUsedAt = &row.LastUsedAt.Time
		}
		view.Tokens = append(view.Tokens, v)
	}
	return view, nil
}

func validManagementTokenLabel(label string) bool {
	if label == "" || !utf8.ValidString(label) || utf8.RuneCountInString(label) > managementTokenLabelMax {
		return false
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (h *Handler) managementTokenCreateHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	rows, risky, err := h.managementAccess(r.Context(), session.User.ID, tenantID, projectID)
	if err != nil {
		webutil.InternalError(w, "management tokens: scopes", err)
		return
	}
	// Check each option again on the POST: the form disables the options
	// that the user cannot give, but a crafted request can send any value.
	scopes, err := scopesFromAccessForm(r.Form, rows, risky)
	if err != nil {
		http.Error(w, "forbidden: "+err.Error(), http.StatusForbidden)
		return
	}

	label := strings.TrimSpace(r.Form.Get("label"))
	daysRaw := strings.TrimSpace(r.Form.Get("days"))
	fieldErrs := map[string]string{}
	if !validManagementTokenLabel(label) {
		fieldErrs["label"] = fmt.Sprintf("Label must be 1–%d characters and cannot contain control characters.", managementTokenLabelMax)
	}
	days := managementTokenDefaultDays
	if daysRaw != "" {
		days, err = strconv.Atoi(daysRaw)
		if err != nil || days < 1 || days > h.managementMaxDays() {
			fieldErrs["days"] = fmt.Sprintf("Days must be a whole number from 1 to %d.", h.managementMaxDays())
		}
	}
	if len(fieldErrs) > 0 {
		h.renderManagementTokensError(w, r, tenantID, projectID, http.StatusUnprocessableEntity, "", fieldErrs, label, daysRaw)
		return
	}

	token, err := newManagementTokenValue()
	if err != nil {
		webutil.InternalError(w, "management tokens: generate", err)
		return
	}
	expiresAt := time.Now().Add(time.Duration(days) * 24 * time.Hour)
	err = h.createManagementToken(r.Context(), tenantID, projectID, session.User.ID, label, token, scopes, expiresAt)
	switch {
	case errors.Is(err, errManagementTokenLimit):
		h.renderManagementTokensError(w, r, tenantID, projectID, http.StatusConflict,
			fmt.Sprintf("This Game Project already has %d active management tokens. Revoke one first.", h.managementMaxTokens()), nil, label, daysRaw)
		return
	case errors.Is(err, pgx.ErrNoRows):
		http.NotFound(w, r)
		return
	case err != nil:
		webutil.InternalError(w, "management tokens: create", err)
		return
	}

	view, err := h.managementTokensView(r.Context(), tenantID, projectID, nil)
	if err != nil {
		webutil.InternalError(w, "management tokens: load", err)
		return
	}
	view.NewToken = token
	view.NewTokenAccess, _ = accessWords(scopes)
	// The page holds the token value: never cache it.
	w.Header().Set("Cache-Control", "no-store")
	webutil.Render(r, w, ManagementTokensPage(view))
}

// renderManagementTokensError renders the page with the posted form values
// filled in again.
func (h *Handler) renderManagementTokensError(w http.ResponseWriter, r *http.Request, tenantID, projectID int64, status int, msg string, fieldErrs map[string]string, label, days string) {
	view, err := h.managementTokensView(r.Context(), tenantID, projectID, r.Form)
	if err != nil {
		webutil.InternalError(w, "management tokens: load", err)
		return
	}
	view.Error, view.FieldErrors, view.Label, view.Days = msg, fieldErrs, label, days
	w.WriteHeader(status)
	webutil.Render(r, w, ManagementTokensPage(view))
}

// newManagementTokenValue returns ggm_ plus 32 random bytes, base64url
// encoded.
func newManagementTokenValue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return managementtoken.TokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// createManagementToken inserts the token under a project row lock, so two
// creates cannot both pass the active-token limit, and writes the audit row
// in the same transaction.
func (h *Handler) createManagementToken(ctx context.Context, tenantID, projectID, userID int64, label, token string, scopes []string, expiresAt time.Time) error {
	sum := sha256.Sum256([]byte(token))
	if scopes == nil {
		scopes = []string{} // a nil slice is sent as NULL
	}
	ctx = db.WithTenant(ctx, tenantID)
	return h.pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if _, err := q.LockLiveProject(ctx, projectID); err != nil {
			return err
		}
		n, err := q.CountActiveManagementTokens(ctx, projectID)
		if err != nil {
			return err
		}
		if n >= h.managementMaxTokens() {
			return errManagementTokenLimit
		}
		id, err := q.CreateManagementToken(ctx, sqlcgen.CreateManagementTokenParams{
			ProjectID:       projectID,
			CreatedByUserID: userID,
			Label:           label,
			TokenHash:       sum[:],
			TokenHint:       token[len(token)-4:],
			Scopes:          scopes,
			ExpiresAt:       pgtype.Timestamptz{Time: expiresAt, Valid: true},
		})
		if err != nil {
			return err
		}
		return auditlog.WritePlatform(ctx, tx, userID, "management_token.create", strconv.FormatInt(id, 10), map[string]any{
			"tenant_id":  tenantID,
			"project_id": projectID,
			"label":      label,
			"scopes":     scopes,
			"expires_at": expiresAt.UTC(),
		})
	})
}

// managementTokenRevokeHandler revokes a token. The creator can revoke their
// own token; anyone else needs (api_key:secret, manage) in the tenant.
func (h *Handler) managementTokenRevokeHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	tokenID, ok := parsePathID(w, r, "tokenID")
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	ctx := db.WithTenant(r.Context(), tenantID)
	var creator int64
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		var err error
		creator, err = sqlcgen.New(tx).GetManagementTokenCreator(ctx, sqlcgen.GetManagementTokenCreatorParams{ProjectID: projectID, ID: tokenID})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		webutil.InternalError(w, "management tokens: lookup", err)
		return
	}
	if creator != session.User.ID &&
		!h.requireControlPanelPermission(w, r, tenantID, rbac.ObjectAPIKeySecret, rbac.ActionManage) {
		return
	}
	err = h.pool.Q(ctx, func(tx pgx.Tx) error {
		n, err := sqlcgen.New(tx).RevokeManagementToken(ctx, sqlcgen.RevokeManagementTokenParams{ProjectID: projectID, ID: tokenID})
		if err != nil {
			return err
		}
		if n == 0 {
			return pgx.ErrNoRows
		}
		return auditlog.WritePlatform(ctx, tx, session.User.ID, "management_token.revoke", strconv.FormatInt(tokenID, 10), map[string]any{
			"tenant_id":          tenantID,
			"project_id":         projectID,
			"created_by_user_id": creator,
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "management token revoke failed", "err", err)
		http.Error(w, "revoke failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, managementTokensPath(tenantID, projectID)+queryFlash+url.QueryEscape("Management token revoked."), http.StatusSeeOther)
}
