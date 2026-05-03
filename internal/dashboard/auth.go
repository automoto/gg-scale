package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

const (
	sessionCookieName = "ggscale_dashboard_session"
	sessionTTL        = 12 * time.Hour
	sessionSlideAfter = time.Hour
)

var errInvalidSession = errors.New("dashboard: invalid session")

func (h *Handler) issueSession(ctx context.Context, w http.ResponseWriter, userID int64, ip, userAgent string) (dashboardSession, error) {
	refreshToken, err := randomToken()
	if err != nil {
		return dashboardSession{}, err
	}
	csrfSecret, err := randomTokenBytes()
	if err != nil {
		return dashboardSession{}, err
	}
	refreshHash := sha256.Sum256([]byte(refreshToken))
	expires := h.now().Add(sessionTTL)

	var row sqlcgen.CreateDashboardSessionRow
	err = h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).CreateDashboardSession(ctx, sqlcgen.CreateDashboardSessionParams{
			DashboardUserID: userID,
			RefreshHash:     refreshHash[:],
			CsrfSecret:      csrfSecret,
			ExpiresAt:       pgtype.Timestamptz{Time: expires, Valid: true},
			Ip:              optionalString(ip),
			UserAgent:       optionalString(userAgent),
		})
		if err != nil {
			return fmt.Errorf("create dashboard session: %w", err)
		}
		return nil
	})
	if err != nil {
		return dashboardSession{}, err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    refreshToken,
		Path:     "/v1/dashboard",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
	return dashboardSession{
		ID:        row.ID,
		CSRFToken: base64.RawURLEncoding.EncodeToString(csrfSecret),
		ExpiresAt: expires,
	}, nil
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/v1/dashboard",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
}

func (h *Handler) lookupSession(ctx context.Context, cookieValue string) (dashboardSession, error) {
	if cookieValue == "" {
		return dashboardSession{}, errInvalidSession
	}
	refreshHash := sha256.Sum256([]byte(cookieValue))
	now := h.now()

	var out dashboardSession
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).GetDashboardSessionByRefreshHash(ctx, refreshHash[:])
		if err != nil {
			return err
		}
		if row.RevokedAt.Valid || !row.ExpiresAt.Valid || !now.Before(row.ExpiresAt.Time) {
			return errInvalidSession
		}
		out = dashboardSession{
			ID: row.ID,
			User: dashboardUser{
				ID:              row.DashboardUserID,
				Email:           row.Email,
				IsPlatformAdmin: row.IsPlatformAdmin,
			},
			CSRFToken: base64.RawURLEncoding.EncodeToString(row.CsrfSecret),
			ExpiresAt: row.ExpiresAt.Time,
		}
		if row.ExpiresAt.Time.Sub(now) > sessionSlideAfter {
			return nil
		}
		return sqlcgen.New(tx).TouchDashboardSession(ctx, sqlcgen.TouchDashboardSessionParams{
			ID:        row.ID,
			ExpiresAt: pgtype.Timestamptz{Time: now.Add(sessionTTL), Valid: true},
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return dashboardSession{}, errInvalidSession
	}
	if err != nil {
		return dashboardSession{}, err
	}
	return out, nil
}

func (h *Handler) revokeSession(ctx context.Context, sessionID int64) error {
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).RevokeDashboardSession(ctx, sessionID)
	})
}

func (h *Handler) sessionFromRequest(r *http.Request) (dashboardSession, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return dashboardSession{}, false
	}
	session, err := h.lookupSession(r.Context(), cookie.Value)
	return session, err == nil
}

func randomToken() (string, error) {
	b, err := randomTokenBytes()
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func randomTokenBytes() ([]byte, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("dashboard token rand: %w", err)
	}
	return b[:], nil
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
