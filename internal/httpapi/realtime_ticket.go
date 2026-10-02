package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/automoto/gg-scale/internal/auth"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/playerauth"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/tenant"
)

// realtimeTicketTTL is how long a ticket can wait before the WebSocket
// opens. A client asks for a ticket just before it connects.
const realtimeTicketTTL = 30 * time.Second

type realtimeTicketOutput struct {
	Status int
	Body   struct {
		Ticket           string `json:"ticket" doc:"One-time ticket. Open /v1/ws?ticket=<ticket> with it."`
		ExpiresInSeconds int    `json:"expires_in_seconds" doc:"The ticket expires after this many seconds."`
	}
}

// registerRealtimeTicket adds POST /v1/ws/ticket. A browser cannot set the
// Authorization and X-Session-Token headers on a WebSocket, so it gets a
// one-time ticket here with its normal headers and puts the ticket in the
// /v1/ws URL. A ticket in a log is of no use: it works once, for 30 seconds.
func registerRealtimeTicket(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID:   "createRealtimeTicket",
		Method:        http.MethodPost,
		Path:          "/v1/ws/ticket",
		Summary:       "Get a one-time ticket for the realtime WebSocket",
		Description:   "For browser clients, which cannot set headers on a WebSocket. Open /v1/ws?ticket=<ticket> within 30 seconds. The ticket works once.",
		Tags:          []string{"Realtime"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, _ *struct{}) (*realtimeTicketOutput, error) {
		key, okKey := tenant.APIKeyFromContext(ctx)
		playerID, okPlayer := playerauth.IDFromContext(ctx)
		epoch, okEpoch := playerauth.SessionEpochFromContext(ctx)
		if !okKey || !okPlayer || !okEpoch || len(key.Hash) == 0 {
			return nil, huma.Error401Unauthorized("no player")
		}
		projectClaim, _ := playerauth.ProjectIDFromContext(ctx)

		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, serverError(ctx, "realtime ticket: rand", err)
		}
		ticket := base64.RawURLEncoding.EncodeToString(raw[:])
		sum := sha256.Sum256([]byte(ticket))
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			return sqlcgen.New(tx).CreateRealtimeTicket(ctx, sqlcgen.CreateRealtimeTicketParams{
				TicketHash:   sum[:],
				ApiKeyHash:   key.Hash,
				PlayerID:     playerID,
				ProjectID:    projectClaim,
				SessionEpoch: epoch,
				ExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(realtimeTicketTTL), Valid: true},
			})
		})
		if err != nil {
			return nil, serverError(ctx, "realtime ticket: create", err)
		}
		out := &realtimeTicketOutput{Status: http.StatusCreated}
		out.Body.Ticket = ticket
		out.Body.ExpiresInSeconds = int(realtimeTicketTTL.Seconds())
		return out, nil
	})
}

// realtimeTicketAuth authenticates /v1/ws?ticket=... It uses the ticket
// once, resolves the stored API key hash again, and runs the same tenant and
// session checks as the header path (tenant.Admit, playerauth.Admit). So a
// key revoked, a tenant disabled, or a player banned after the ticket was
// made is still refused.
func realtimeTicketAuth(d Deps) func(http.Handler) http.Handler {
	validator := epochValidator{d.Pool}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			sum := sha256.Sum256([]byte(r.URL.Query().Get("ticket")))
			var row sqlcgen.RedeemRealtimeTicketRow
			err := d.Pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
				var err error
				row, err = sqlcgen.New(tx).RedeemRealtimeTicket(ctx, sum[:])
				return err
			})
			if errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if err != nil {
				slog.ErrorContext(ctx, "realtime ticket: redeem", "err", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			ctx, status := tenant.Admit(ctx, d.Lookup, row.ApiKeyHash)
			if status != 0 {
				tenant.WriteRefusal(w, status)
				return
			}
			key, _ := tenant.APIKeyFromContext(ctx)
			claims := auth.Claims{
				PlayerID:     row.PlayerID,
				TenantID:     row.TenantID,
				ProjectID:    row.ProjectID,
				SessionEpoch: row.SessionEpoch,
			}
			ctx, refusal := playerauth.Admit(ctx, key.TenantID, claims, validator)
			if refusal.Status != 0 {
				refusal.Write(w)
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// realtimeRoute serves GET /v1/ws for both kinds of client. A native client
// sends the Authorization and X-Session-Token headers; a browser sends a
// ticket. The header path keeps the middleware order of the other player
// routes. The ticket path authenticates first, then runs the same two
// limiters (per API key, then per player).
func realtimeRoute(d Deps, ws http.Handler, reg prometheus.Registerer) http.HandlerFunc {
	limited := ratelimit.New(d.Limiter, d.RateLimitOverrides, reg)(
		ratelimit.NewPlayerLimiter(d.Limiter, ratelimit.PlayerRate, ratelimit.PlayerBurst, reg)(ws))
	byHeaders := tenant.New(d.Lookup)(
		ratelimit.New(d.Limiter, d.RateLimitOverrides, reg)(
			playerauth.New(d.Signer, epochValidator{d.Pool})(
				ratelimit.NewPlayerLimiter(d.Limiter, ratelimit.PlayerRate, ratelimit.PlayerBurst, reg)(ws))))
	byTicket := realtimeTicketAuth(d)(limited)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" && r.URL.Query().Has("ticket") {
			byTicket.ServeHTTP(w, r)
			return
		}
		byHeaders.ServeHTTP(w, r)
	}
}
