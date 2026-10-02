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
	"github.com/automoto/gg-scale/internal/mcp"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/webutil"
)

const (
	mcpTokenDefaultDays   = 30
	mcpTokenExpiryWarning = 7 * 24 * time.Hour
	mcpTokenLabelMax      = 64
	presetReadWrite       = "read_write"
)

var errMCPTokenLimit = errors.New("control panel: MCP token limit reached")

// MCPTokensView renders a project's MCP token page.
type MCPTokensView struct {
	UserEmail       string
	CSRFToken       string
	TenantID        int64
	ProjectID       int64
	Tokens          []MCPTokenRowView
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

// MCPTokenRowView is one token in the list. Token values are never stored.
type MCPTokenRowView struct {
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

func mcpTokensPath(tenantID, projectID int64) string {
	return pathTenantsPrefix + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/mcp-tokens"
}

func (h *Handler) requireMCPFeature(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.cfg.MCPEnabled {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) mcpMaxDays() int {
	if h.cfg.MCPMaxExpiryDays > 0 {
		return h.cfg.MCPMaxExpiryDays
	}
	return 365
}

func (h *Handler) mcpMaxTokens() int64 {
	if h.cfg.MCPMaxProjectTokens > 0 {
		return int64(h.cfg.MCPMaxProjectTokens)
	}
	return 20
}

// grantableMCPScopes returns the write scopes whose Casbin pair the user
// passes now. A token can never get a scope its creator could not use.
func (h *Handler) grantableMCPScopes(userID, tenantID, projectID int64) ([]string, error) {
	var out []string
	for _, s := range mcp.WriteScopes {
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

func (h *Handler) mcpTokensPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	view, err := h.mcpTokensView(r.Context(), tenantID, projectID)
	if err != nil {
		webutil.InternalError(w, "mcp tokens: load", err)
		return
	}
	view.Message = r.URL.Query().Get("flash")
	webutil.Render(r, w, MCPTokensPage(view))
}

func (h *Handler) mcpTokensView(ctx context.Context, tenantID, projectID int64) (MCPTokensView, error) {
	session, _ := sessionFromContext(ctx)
	view := MCPTokensView{
		UserEmail:   session.User.Email,
		CSRFToken:   session.CSRFToken,
		TenantID:    tenantID,
		ProjectID:   projectID,
		MaxDays:     h.mcpMaxDays(),
		DefaultDays: mcpTokenDefaultDays,
		Days:        strconv.Itoa(mcpTokenDefaultDays),
	}
	var err error
	if view.GrantableScopes, err = h.grantableMCPScopes(session.User.ID, tenantID, projectID); err != nil {
		return view, err
	}
	var rows []sqlcgen.ListMCPTokensForProjectRow
	tctx := db.WithTenant(ctx, tenantID)
	err = h.pool.Q(tctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlcgen.New(tx).ListMCPTokensForProject(tctx, projectID)
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
		v := MCPTokenRowView{
			ID:           row.ID,
			Label:        row.Label,
			Hint:         row.TokenHint,
			Scopes:       row.Scopes,
			CreatorEmail: row.CreatorEmail,
			ExpiresAt:    row.ExpiresAt.Time,
			Expired:      !row.ExpiresAt.Time.After(now),
			ExpiresSoon:  row.ExpiresAt.Time.Sub(now) <= mcpTokenExpiryWarning,
			Inactive:     !active,
		}
		if row.LastUsedAt.Valid {
			v.LastUsedAt = &row.LastUsedAt.Time
		}
		view.Tokens = append(view.Tokens, v)
	}
	return view, nil
}

func validMCPTokenLabel(label string) bool {
	if label == "" || !utf8.ValidString(label) || utf8.RuneCountInString(label) > mcpTokenLabelMax {
		return false
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (h *Handler) mcpTokenCreateHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	grantable, err := h.grantableMCPScopes(session.User.ID, tenantID, projectID)
	if err != nil {
		webutil.InternalError(w, "mcp tokens: scopes", err)
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
	if r.Form.Get("preset") == presetReadWrite {
		scopes = grantable
	}

	label := strings.TrimSpace(r.Form.Get("label"))
	daysRaw := strings.TrimSpace(r.Form.Get("days"))
	fieldErrs := map[string]string{}
	if !validMCPTokenLabel(label) {
		fieldErrs["label"] = fmt.Sprintf("Label must be 1–%d characters and cannot contain control characters.", mcpTokenLabelMax)
	}
	days := mcpTokenDefaultDays
	if daysRaw != "" {
		days, err = strconv.Atoi(daysRaw)
		if err != nil || days < 1 || days > h.mcpMaxDays() {
			fieldErrs["days"] = fmt.Sprintf("Days must be a whole number from 1 to %d.", h.mcpMaxDays())
		}
	}
	if len(fieldErrs) > 0 {
		h.renderMCPTokensError(w, r, tenantID, projectID, http.StatusUnprocessableEntity, "", fieldErrs, label, daysRaw)
		return
	}

	token, err := newMCPTokenValue()
	if err != nil {
		webutil.InternalError(w, "mcp tokens: generate", err)
		return
	}
	expiresAt := time.Now().Add(time.Duration(days) * 24 * time.Hour)
	err = h.createMCPToken(r.Context(), tenantID, projectID, session.User.ID, label, token, scopes, expiresAt)
	switch {
	case errors.Is(err, errMCPTokenLimit):
		h.renderMCPTokensError(w, r, tenantID, projectID, http.StatusConflict,
			fmt.Sprintf("This Game Project already has %d active MCP tokens. Revoke one first.", h.mcpMaxTokens()), nil, label, daysRaw)
		return
	case errors.Is(err, pgx.ErrNoRows):
		http.NotFound(w, r)
		return
	case err != nil:
		webutil.InternalError(w, "mcp tokens: create", err)
		return
	}

	view, err := h.mcpTokensView(r.Context(), tenantID, projectID)
	if err != nil {
		webutil.InternalError(w, "mcp tokens: load", err)
		return
	}
	view.NewToken = token
	// The page holds the token value: never cache it.
	w.Header().Set("Cache-Control", "no-store")
	webutil.Render(r, w, MCPTokensPage(view))
}

func (h *Handler) renderMCPTokensError(w http.ResponseWriter, r *http.Request, tenantID, projectID int64, status int, msg string, fieldErrs map[string]string, label, days string) {
	view, err := h.mcpTokensView(r.Context(), tenantID, projectID)
	if err != nil {
		webutil.InternalError(w, "mcp tokens: load", err)
		return
	}
	view.Error, view.FieldErrors, view.Label, view.Days = msg, fieldErrs, label, days
	w.WriteHeader(status)
	webutil.Render(r, w, MCPTokensPage(view))
}

// newMCPTokenValue returns ggm_ plus 32 random bytes, base64url encoded.
func newMCPTokenValue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return mcp.TokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// createMCPToken inserts the token under a project row lock, so two creates
// cannot both pass the active-token limit, and writes the audit row in the
// same transaction.
func (h *Handler) createMCPToken(ctx context.Context, tenantID, projectID, userID int64, label, token string, scopes []string, expiresAt time.Time) error {
	sum := sha256.Sum256([]byte(token))
	if scopes == nil {
		scopes = []string{} // a nil slice is sent as NULL
	}
	ctx = db.WithTenant(ctx, tenantID)
	return h.pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if _, err := q.LockProjectForMCPTokenCreate(ctx, projectID); err != nil {
			return err
		}
		n, err := q.CountActiveMCPTokens(ctx, projectID)
		if err != nil {
			return err
		}
		if n >= h.mcpMaxTokens() {
			return errMCPTokenLimit
		}
		id, err := q.CreateMCPToken(ctx, sqlcgen.CreateMCPTokenParams{
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
		return auditlog.WritePlatform(ctx, tx, userID, "mcp_token.create", strconv.FormatInt(id, 10), map[string]any{
			"tenant_id":  tenantID,
			"project_id": projectID,
			"label":      label,
			"scopes":     scopes,
			"expires_at": expiresAt.UTC(),
		})
	})
}

// mcpTokenRevokeHandler revokes a token. The creator can revoke their own
// token; anyone else needs (api_key:secret, manage) in the tenant.
func (h *Handler) mcpTokenRevokeHandler(w http.ResponseWriter, r *http.Request) {
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
		creator, err = sqlcgen.New(tx).GetMCPTokenCreator(ctx, sqlcgen.GetMCPTokenCreatorParams{ProjectID: projectID, ID: tokenID})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		webutil.InternalError(w, "mcp tokens: lookup", err)
		return
	}
	if creator != session.User.ID &&
		!h.requireControlPanelPermission(w, r, tenantID, rbac.ObjectAPIKeySecret, rbac.ActionManage) {
		return
	}
	err = h.pool.Q(ctx, func(tx pgx.Tx) error {
		n, err := sqlcgen.New(tx).RevokeMCPToken(ctx, sqlcgen.RevokeMCPTokenParams{ProjectID: projectID, ID: tokenID})
		if err != nil {
			return err
		}
		if n == 0 {
			return pgx.ErrNoRows
		}
		return auditlog.WritePlatform(ctx, tx, session.User.ID, "mcp_token.revoke", strconv.FormatInt(tokenID, 10), map[string]any{
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
		slog.ErrorContext(r.Context(), "mcp token revoke failed", "err", err)
		http.Error(w, "revoke failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, mcpTokensPath(tenantID, projectID)+queryFlash+url.QueryEscape("MCP token revoked."), http.StatusSeeOther)
}
