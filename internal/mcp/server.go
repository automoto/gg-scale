// Package mcp serves the Model Context Protocol endpoint (POST /mcp) for
// coding agents. The protocol is the official Go SDK in stateless streamable
// HTTP mode with JSON responses. This package adds the token checks, the
// limits, and the tools.
//
// A request authenticates with an MCP token (ggm_...). The token is its own
// principal for one project. The person who created it is an upper limit on
// what it can do, checked on each request, never a source of rights.
package mcp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/auth"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/rbac"
)

// TokenPrefix starts each MCP token value.
const TokenPrefix = "ggm_"

const (
	maxBodyBytes      = 128 << 10
	lastUsedPrecision = time.Minute
	serverName        = "ggscale"
)

// Deps are the values the handler needs.
type Deps struct {
	Pool       *db.Pool
	RBAC       *rbac.Authorizer
	Limiter    ratelimit.Limiter
	ProxyTrust *ratelimit.ProxyTrust
	Version    string
	// TokenRatePerSecond / TokenBurst set the per-token bucket.
	TokenRatePerSecond float64
	TokenBurst         float64
	// Facts for project_health_check. None of them is secret.
	FleetEnabled       bool
	RelayEnabled       bool
	RelayConfigured    bool
	CORSAllowedOrigins []string
	// MaxProjectAPIKeys and MaxProjectOrigins limit the write tools.
	MaxProjectAPIKeys int64
	MaxProjectOrigins int
	Now               func() time.Time
}

// Principal is the authenticated token. It holds no dashboard session.
type Principal struct {
	TokenID         int64
	TenantID        int64
	ProjectID       int64
	CreatedByUserID int64
	Label           string
	Scopes          []string
	ExpiresAt       time.Time
}

// HasScope reports whether the token has scope.
func (p Principal) HasScope(scope string) bool {
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

type handler struct {
	d   Deps
	sdk http.Handler
}

type principalKey struct{}

// principalExtra is the TokenInfo.Extra key that carries the Principal to
// the tool handlers.
const principalExtra = "ggscale.principal"

// New returns the /mcp handler. The SDK server and its tools are built once.
func New(d Deps) http.Handler {
	return newHandler(d)
}

func newHandler(d Deps) *handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.MaxProjectAPIKeys <= 0 {
		d.MaxProjectAPIKeys = 20
	}
	if d.MaxProjectOrigins <= 0 {
		d.MaxProjectOrigins = 20
	}
	h := &handler{d: d}
	srv := h.newServer()
	streamable := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv },
		&mcpsdk.StreamableHTTPOptions{
			Stateless:           true,
			JSONResponse:        true,
			MaxRequestBodyBytes: maxBodyBytes,
		})
	// ServeHTTP has already authenticated the request. RequireBearerToken is
	// the SDK's way to give each tool call the caller (req.Extra.TokenInfo).
	h.sdk = auth.RequireBearerToken(passPrincipal, nil)(streamable)
	return h
}

// passPrincipal hands the Principal that ServeHTTP put on the context to the
// SDK as TokenInfo.
func passPrincipal(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	if !ok {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{
		Scopes:     p.Scopes,
		Expiration: p.ExpiresAt,
		Extra:      map[string]any{principalExtra: p},
	}, nil
}

// principalFrom returns the caller of a tool call or tools/list request.
func principalFrom(extra *mcpsdk.RequestExtra) (Principal, bool) {
	if extra == nil || extra.TokenInfo == nil {
		return Principal{}, false
	}
	p, ok := extra.TokenInfo.Extra[principalExtra].(Principal)
	return p, ok
}

// authError is a refusal before the SDK reads the request.
type authError struct {
	status int
	msg    string
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	p, aerr := h.authenticate(r)
	if aerr != nil {
		if aerr.status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", "Bearer")
		}
		http.Error(w, aerr.msg, aerr.status)
		return
	}
	h.sdk.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
}

// sameOrigin refuses a browser request from another site. Agents of this
// release send no Origin header.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

// authenticate runs the request checks in order and stops at the first
// failure. The per-IP bucket is debited before the token lookup and refunded
// only when the token passes every identity check, so a wrong token, and a
// real token that no longer works, both use up the bucket. The per-token
// limiter runs after the refund: an agent that only goes over its own rate
// does not use up the per-IP bucket.
func (h *handler) authenticate(r *http.Request) (Principal, *authError) {
	ctx := r.Context()
	ipBucket := "ratelimit:ip:mcp:" + h.d.ProxyTrust.ClientIP(r)
	decision, err := h.d.Limiter.Allow(ctx, ipBucket, ratelimit.AuthIPRate, ratelimit.AuthIPBurst)
	if err != nil {
		return Principal{}, &authError{http.StatusInternalServerError, "internal error"}
	}
	if !decision.Allowed {
		return Principal{}, &authError{http.StatusTooManyRequests, "too many failed authentication attempts"}
	}

	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		return Principal{}, &authError{http.StatusUnauthorized, "unauthorized"}
	}
	sum := sha256.Sum256([]byte(token))
	var row sqlcgen.GetMCPTokenByHashRow
	err = h.d.Pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		row, err = sqlcgen.New(tx).GetMCPTokenByHash(ctx, sum[:])
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows), err == nil && row.RevokedAt.Valid:
		return Principal{}, &authError{http.StatusUnauthorized, "unauthorized"}
	case err != nil:
		slog.ErrorContext(ctx, "mcp: token lookup", "err", err)
		return Principal{}, &authError{http.StatusInternalServerError, "internal error"}
	case !row.ExpiresAt.Time.After(h.d.Now()):
		// A distinct message, so the agent can tell the developer to make a
		// new token.
		return Principal{}, &authError{http.StatusUnauthorized, "token expired"}
	}

	p := Principal{
		TokenID:         row.ID,
		TenantID:        row.TenantID,
		ProjectID:       row.ProjectID,
		CreatedByUserID: row.CreatedByUserID,
		Label:           row.Label,
		Scopes:          row.Scopes,
		ExpiresAt:       row.ExpiresAt.Time,
	}
	if row.TenantDisabled || row.CreatorDisabled {
		return Principal{}, &authError{http.StatusForbidden, "forbidden"}
	}
	var live bool
	tctx := db.WithTenant(ctx, p.TenantID)
	err = h.d.Pool.Q(tctx, func(tx pgx.Tx) error {
		var err error
		live, err = sqlcgen.New(tx).ProjectIsLive(tctx, p.ProjectID)
		return err
	})
	if err != nil {
		slog.ErrorContext(ctx, "mcp: project check", "err", err)
		return Principal{}, &authError{http.StatusInternalServerError, "internal error"}
	}
	if !live {
		return Principal{}, &authError{http.StatusForbidden, "forbidden"}
	}
	// The creator limit: the creator must still manage the tenant's projects.
	ok, err = h.d.RBAC.CanControlPanel(p.CreatedByUserID, p.TenantID, rbac.ObjectProject, rbac.ActionManage)
	if err != nil {
		return Principal{}, &authError{http.StatusInternalServerError, "internal error"}
	}
	if !ok {
		return Principal{}, &authError{http.StatusForbidden, "the person who created this token can no longer manage this Game Project"}
	}

	if refunder, ok := h.d.Limiter.(ratelimit.Refunder); ok {
		if err := refunder.Refund(ctx, ipBucket, ratelimit.AuthIPRate, ratelimit.AuthIPBurst); err != nil {
			slog.WarnContext(ctx, "mcp: refund per-IP bucket", "err", err)
		}
	}
	decision, err = h.d.Limiter.Allow(ctx, "ratelimit:mcp:"+strconv.FormatInt(p.TokenID, 10), h.d.TokenRatePerSecond, h.d.TokenBurst)
	if err != nil {
		return Principal{}, &authError{http.StatusInternalServerError, "internal error"}
	}
	if !decision.Allowed {
		return Principal{}, &authError{http.StatusTooManyRequests,
			fmt.Sprintf("rate limit exceeded; retry in %d seconds", int(math.Ceil(decision.RetryAfter.Seconds())))}
	}

	// Only a request that is let through counts as a use, at most one write
	// per minute for each token. A failed write does not refuse the request.
	if !row.LastUsedAt.Valid || h.d.Now().Sub(row.LastUsedAt.Time) >= lastUsedPrecision {
		if err := h.d.Pool.Q(tctx, func(tx pgx.Tx) error {
			return sqlcgen.New(tx).TouchMCPTokenLastUsed(tctx, p.TokenID)
		}); err != nil {
			slog.WarnContext(ctx, "mcp: touch last_used_at", "err", err)
		}
	}
	return p, nil
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(header[len(prefix):])
	return tok, tok != ""
}

const serverInstructions = "ggscale backend for one Game Project. Use project_health_check first " +
	"to find setup problems. All tools act on the Game Project of the token; no tool takes a project ID."
