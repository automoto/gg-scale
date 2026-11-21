package managementtoken

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

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/rbac"
)

const lastUsedPrecision = time.Minute

// Deps are the values the token middleware needs.
type Deps struct {
	Pool       *db.Pool
	RBAC       *rbac.Authorizer
	Limiter    ratelimit.Limiter
	ProxyTrust *ratelimit.ProxyTrust
	// TokenRatePerSecond / TokenBurst set each per-token bucket.
	TokenRatePerSecond float64
	TokenBurst         float64
	// AuthFailuresPerMinute / AuthFailureBurst set the per-IP bucket that
	// only failed authentications use. Defaults 10 / 10.
	AuthFailuresPerMinute float64
	AuthFailureBurst      float64
	Now                   func() time.Time
}

type principalKey struct{}

// NewContext returns ctx with p as the authenticated token.
func NewContext(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the token that Middleware authenticated.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// SameOrigin refuses a browser request from another site. Agents and API
// clients send no Origin header.
func SameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

// Middleware authenticates the bearer token and puts its Principal on the
// request context. bucket names the per-token rate limit bucket, so each
// route group has its own limit. The per-IP failure bucket is shared.
func Middleware(d Deps, bucket string) func(http.Handler) http.Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.AuthFailuresPerMinute <= 0 {
		d.AuthFailuresPerMinute = 10
	}
	if d.AuthFailureBurst <= 0 {
		d.AuthFailureBurst = 10
	}
	a := authenticator{d: d, bucket: bucket}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, aerr := a.authenticate(r)
			if aerr != nil && aerr.status != http.StatusInternalServerError {
				aerr = a.chargeFailure(r, aerr)
			}
			if aerr != nil {
				if aerr.status == http.StatusUnauthorized {
					w.Header().Set("WWW-Authenticate", "Bearer")
				}
				http.Error(w, aerr.msg, aerr.status)
				return
			}
			next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), p)))
		})
	}
}

type authenticator struct {
	d      Deps
	bucket string
}

// authError is a refusal before the next handler reads the request.
type authError struct {
	status int
	msg    string
}

// chargeFailure debits the per-IP bucket for a failed authentication: a wrong
// token, and a real token that no longer works. Only failures use the bucket,
// so clients that send bad tokens from a shared IP never block valid tokens.
// A token cannot be guessed, and an unknown token costs one indexed lookup,
// so letting an over-limit IP still reach the lookup is cheap.
func (a authenticator) chargeFailure(r *http.Request, aerr *authError) *authError {
	if aerr.status == http.StatusTooManyRequests {
		// The per-token limit: the token itself is valid.
		return aerr
	}
	ctx := r.Context()
	decision, err := a.d.Limiter.Allow(ctx, "ratelimit:ip:management_token:"+a.d.ProxyTrust.ClientIP(r),
		a.d.AuthFailuresPerMinute/60, a.d.AuthFailureBurst)
	if err != nil {
		slog.ErrorContext(ctx, "management token: per-IP bucket", "err", err)
		return &authError{http.StatusInternalServerError, "internal error"}
	}
	if !decision.Allowed {
		return &authError{http.StatusTooManyRequests, "too many failed authentication attempts"}
	}
	return aerr
}

// authenticate runs the request checks in order and stops at the first
// failure. The per-token limiter runs last, so a client that only goes over
// its own rate does not use up the per-IP failure bucket.
func (a authenticator) authenticate(r *http.Request) (Principal, *authError) {
	ctx := r.Context()
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		return Principal{}, &authError{http.StatusUnauthorized, "unauthorized"}
	}
	sum := sha256.Sum256([]byte(token))
	var row sqlcgen.GetManagementTokenByHashRow
	err := a.d.Pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).GetManagementTokenByHash(ctx, sum[:])
		return qerr
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows), err == nil && row.RevokedAt.Valid:
		return Principal{}, &authError{http.StatusUnauthorized, "unauthorized"}
	case err != nil:
		slog.ErrorContext(ctx, "management token: token lookup", "err", err)
		return Principal{}, &authError{http.StatusInternalServerError, "internal error"}
	case !row.ExpiresAt.Time.After(a.d.Now()):
		// A distinct message, so the client can tell the developer to make a
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
	}
	if row.TenantDisabled || row.CreatorDisabled {
		return Principal{}, &authError{http.StatusForbidden, "forbidden"}
	}
	var live bool
	tctx := db.WithTenant(ctx, p.TenantID)
	err = a.d.Pool.Q(tctx, func(tx pgx.Tx) error {
		var err error
		live, err = sqlcgen.New(tx).ProjectIsLive(tctx, p.ProjectID)
		return err
	})
	if err != nil {
		slog.ErrorContext(ctx, "management token: project check", "err", err)
		return Principal{}, &authError{http.StatusInternalServerError, "internal error"}
	}
	if !live {
		return Principal{}, &authError{http.StatusForbidden, "forbidden"}
	}
	// The creator limit: the creator must still manage the tenant's projects.
	ok, err = a.d.RBAC.CanControlPanel(p.CreatedByUserID, p.TenantID, rbac.ObjectProject, rbac.ActionManage)
	if err != nil {
		return Principal{}, &authError{http.StatusInternalServerError, "internal error"}
	}
	if !ok {
		return Principal{}, &authError{http.StatusForbidden, "the person who created this token can no longer manage this Game Project"}
	}

	key := "ratelimit:management_token:" + a.bucket + ":" + strconv.FormatInt(p.TokenID, 10)
	decision, err := a.d.Limiter.Allow(ctx, key, a.d.TokenRatePerSecond, a.d.TokenBurst)
	if err != nil {
		return Principal{}, &authError{http.StatusInternalServerError, "internal error"}
	}
	if !decision.Allowed {
		return Principal{}, &authError{http.StatusTooManyRequests,
			fmt.Sprintf("rate limit exceeded; retry in %d seconds", int(math.Ceil(decision.RetryAfter.Seconds())))}
	}

	// Only a request that is let through counts as a use, at most one write
	// per minute for each token. A failed write does not refuse the request.
	if !row.LastUsedAt.Valid || a.d.Now().Sub(row.LastUsedAt.Time) >= lastUsedPrecision {
		if err := a.d.Pool.Q(tctx, func(tx pgx.Tx) error {
			return sqlcgen.New(tx).TouchManagementTokenLastUsed(tctx, p.TokenID)
		}); err != nil {
			slog.WarnContext(ctx, "management token: touch last_used_at", "err", err)
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
