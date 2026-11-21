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
	presetReadWrite              = "read_write"
	presetCustom                 = "custom"
)

var errManagementTokenLimit = errors.New("control panel: management token limit reached")

// ManagementTokensView renders a project's management token page.
type ManagementTokensView struct {
	UserEmail       string
	CSRFToken       string
	TenantID        int64
	ProjectID       int64
	Tokens          []ManagementTokenRowView
	GrantableScopes []string
	MaxDays         int
	DefaultDays     int
	// NewToken is the token value, shown once right after creation.
	NewToken    string
	Message     string
	Error       string
	FieldErrors map[string]string
	Label       string
	Days        string
}

// ManagementTokenRowView is one token in the list. Token values are never
// stored.
type ManagementTokenRowView struct {
	ID           int64
	Label        string
	Hint         string
	Scopes       []string
	CreatorEmail string
	ExpiresAt    time.Time
	ExpiresSoon  bool
	Expired      bool
	LastUsedAt   *time.Time
	// Inactive means the creator no longer passes (project, manage), so
	// each request with the token is refused.
	Inactive bool
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

// grantableManagementScopes returns the write scopes whose Casbin pair the
// user passes now. A token can never get a scope its creator could not use.
func (h *Handler) grantableManagementScopes(userID, tenantID, projectID int64) ([]string, error) {
	var out []string
	for _, s := range managementtoken.WriteScopes {
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

func (h *Handler) managementTokensPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	view, err := h.managementTokensView(r.Context(), tenantID, projectID)
	if err != nil {
		webutil.InternalError(w, "management tokens: load", err)
		return
	}
	view.Message = r.URL.Query().Get("flash")
	webutil.Render(r, w, ManagementTokensPage(view))
}

func (h *Handler) managementTokensView(ctx context.Context, tenantID, projectID int64) (ManagementTokensView, error) {
	session, _ := sessionFromContext(ctx)
	view := ManagementTokensView{
		UserEmail:   session.User.Email,
		CSRFToken:   session.CSRFToken,
		TenantID:    tenantID,
		ProjectID:   projectID,
		MaxDays:     h.managementMaxDays(),
		DefaultDays: managementTokenDefaultDays,
		Days:        strconv.Itoa(managementTokenDefaultDays),
	}
	var err error
	if view.GrantableScopes, err = h.grantableManagementScopes(session.User.ID, tenantID, projectID); err != nil {
		return view, err
	}
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
		v := ManagementTokenRowView{
			ID:           row.ID,
			Label:        row.Label,
			Hint:         row.TokenHint,
			Scopes:       row.Scopes,
			CreatorEmail: row.CreatorEmail,
			ExpiresAt:    row.ExpiresAt.Time,
			Expired:      !row.ExpiresAt.Time.After(now),
			ExpiresSoon:  row.ExpiresAt.Time.Sub(now) <= managementTokenExpiryWarning,
			Inactive:     !active,
		}
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
	grantable, err := h.grantableManagementScopes(session.User.ID, tenantID, projectID)
	if err != nil {
		webutil.InternalError(w, "management tokens: scopes", err)
		return
	}
	// Check the scopes again on the POST: the form offered only grantable
	// ones, but a crafted request can send any value.
	scopes := []string{}
	for _, s := range r.Form["scopes"] {
		if !slices.Contains(grantable, s) {
			http.Error(w, "forbidden: you cannot grant the scope "+strconv.Quote(s), http.StatusForbidden)
			return
		}
		if !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	// Only the custom preset uses the checked scopes. Any other value,
	// "read_only" included, gives a token with no write scopes.
	switch r.Form.Get("preset") {
	case presetReadWrite:
		scopes = grantable
	case presetCustom:
	default:
		scopes = []string{}
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

	view, err := h.managementTokensView(r.Context(), tenantID, projectID)
	if err != nil {
		webutil.InternalError(w, "management tokens: load", err)
		return
	}
	view.NewToken = token
	// The page holds the token value: never cache it.
	w.Header().Set("Cache-Control", "no-store")
	webutil.Render(r, w, ManagementTokensPage(view))
}

func (h *Handler) renderManagementTokensError(w http.ResponseWriter, r *http.Request, tenantID, projectID int64, status int, msg string, fieldErrs map[string]string, label, days string) {
	view, err := h.managementTokensView(r.Context(), tenantID, projectID)
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
