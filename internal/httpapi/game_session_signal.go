package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/playerauth"
	"github.com/automoto/gg-scale/internal/ratelimit"
)

const (
	maxGameSessionSignalBytes = 65536
	maxSignalsPerMinute       = 30
	// Match the burst to the sustained per-minute allowance so a large session
	// can exchange its initial offers without an artificial ten-signal ceiling.
	signalBurst         = maxSignalsPerMinute
	signalRatePerSecond = maxSignalsPerMinute / 60.0
)

type signalMetrics struct {
	throttled    prometheus.Counter
	refundErrors prometheus.Counter
}

func newSignalMetrics(reg *prometheus.Registry) signalMetrics {
	if reg == nil {
		return signalMetrics{}
	}
	return signalMetrics{
		throttled:    registerSignalCounter(reg, "ggscale_game_session_signal_throttled_total", "Game-session signal sends refused by the in-memory sender limit."),
		refundErrors: registerSignalCounter(reg, "ggscale_game_session_signal_refund_errors_total", "Failed refunds after a debited game-session signal send failed."),
	}
}

func registerSignalCounter(reg prometheus.Registerer, name, help string) prometheus.Counter {
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: help})
	if err := reg.Register(counter); err != nil {
		if already, ok := err.(prometheus.AlreadyRegisteredError); ok {
			return already.ExistingCollector.(prometheus.Counter)
		}
		panic(err)
	}
	return counter
}

func signalRateLimited(retryAfter time.Duration) error {
	retrySeconds := int(math.Ceil(retryAfter.Seconds()))
	if retrySeconds < 1 {
		retrySeconds = 1
	}
	return huma.ErrorWithHeaders(
		huma.Error429TooManyRequests("signal rate limit exceeded"),
		http.Header{"Retry-After": []string{strconv.Itoa(retrySeconds)}},
	)
}

// gameSessionSignalRequest carries one negotiation signal. The field
// constraints are declared as huma struct tags so they appear in the
// generated OpenAPI schema (and are enforced before the handler runs).
// negotiation_id's maxLength is in characters, matching the column's
// char_length CHECK. Payload's cap is a byte limit that JSON Schema cannot
// express (maxLength counts runes), so it is enforced separately against the
// octet_length CHECK — see validateSignalPayloadSize.
type gameSessionSignalRequest struct {
	ToPlayerID    int64  `json:"to_player_id" minimum:"1" example:"87"`
	NegotiationID string `json:"negotiation_id" minLength:"1" maxLength:"128" example:"neg-42-87-1"`
	Kind          string `json:"kind" enum:"offer,answer,restart_offer,restart_answer" example:"offer"`
	Payload       string `json:"payload" minLength:"1" example:"eyJ0eXBlIjoib2ZmZXIiLCJzZHAiOiJ2PTAuLi4ifQ=="`
}

type gameSessionSignalSendInput struct {
	ID   string `path:"id" example:"gs_9f86d081884c7d659a2feaa0c55ad015"`
	Body gameSessionSignalRequest
}

type gameSessionSignalSendResult struct {
	ID int64 `json:"id" example:"512"`
}

type gameSessionSignalSendOutput struct{ Body gameSessionSignalSendResult }

type gameSessionSignalPollInput struct {
	ID      string `path:"id" example:"gs_9f86d081884c7d659a2feaa0c55ad015"`
	AfterID int64  `query:"after_id" minimum:"0" example:"512"`
}

type gameSessionSignalEntry struct {
	ID            int64     `json:"id" example:"513"`
	FromPlayerID  int64     `json:"from_player_id" example:"42"`
	ToPlayerID    int64     `json:"to_player_id" example:"87"`
	NegotiationID string    `json:"negotiation_id" example:"neg-42-87-1"`
	Kind          string    `json:"kind" example:"answer"`
	Payload       string    `json:"payload" example:"eyJ0eXBlIjoiYW5zd2VyIiwic2RwIjoidj0wLi4uIn0="`
	CreatedAt     time.Time `json:"created_at" example:"2026-01-02T15:04:05Z"`
}

type gameSessionSignalPollResult struct {
	Signals []gameSessionSignalEntry `json:"signals"`
}

type gameSessionSignalPollOutput struct{ Body gameSessionSignalPollResult }

func registerGameSessionSignalRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "sendGameSessionSignal", Method: http.MethodPost,
		Path: "/v1/game-session/{id}/signals", Summary: "Send a game-session negotiation signal",
		Tags: []string{"Game Sessions & Invites"}, Security: playerSecurity, DefaultStatus: http.StatusCreated,
	}, gameSessionSignalSend(d))
	huma.Register(api, huma.Operation{
		OperationID: "pollGameSessionSignals", Method: http.MethodGet,
		Path: "/v1/game-session/{id}/signals", Summary: "Poll game-session negotiation signals",
		Tags: []string{"Game Sessions & Invites"}, Security: playerSecurity,
	}, gameSessionSignalPoll(d))
}

// validateSignalPayloadSize enforces the payload's byte bounds, which mirror
// the octet_length CHECK on game_session_signal.payload. The minimum is
// covered by the schema's minLength; this guards the byte ceiling that
// maxLength (a rune count) cannot express.
func validateSignalPayloadSize(payload string) error {
	if len(payload) == 0 || len(payload) > maxGameSessionSignalBytes {
		return fmt.Errorf("payload must contain 1-%d bytes", maxGameSessionSignalBytes)
	}
	return nil
}

func gameSessionSignalSend(d Deps) func(context.Context, *gameSessionSignalSendInput) (*gameSessionSignalSendOutput, error) {
	metrics := newSignalMetrics(d.Registry)
	return func(ctx context.Context, in *gameSessionSignalSendInput) (*gameSessionSignalSendOutput, error) {
		if err := validateSignalPayloadSize(in.Body.Payload); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		fromID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		if fromID == in.Body.ToPlayerID {
			return nil, huma.Error400BadRequest("cannot signal yourself")
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			return nil, huma.Error400BadRequest("api key has no project pin")
		}

		// Debit before opening a transaction: a throttled send performs no
		// Postgres work. Player IDs are globally unique, so this bucket applies
		// per sender across sessions without an advisory lock or history count.
		bucket := fmt.Sprintf("ratelimit:signal:%d", fromID)
		decision, err := d.Limiter.Allow(ctx, bucket, signalRatePerSecond, signalBurst)
		if err != nil {
			return nil, serverError(ctx, "game session signal rate limit", err)
		}
		if !decision.Allowed {
			if metrics.throttled != nil {
				metrics.throttled.Inc()
			}
			return nil, signalRateLimited(decision.RetryAfter)
		}

		var signalID int64
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			active, qerr := q.CountActiveSignalMembers(ctx, sqlcgen.CountActiveSignalMembersParams{
				SessionID: in.ID,
				ProjectID: projectID,
				PlayerIds: []int64{fromID, in.Body.ToPlayerID},
			})
			if qerr != nil {
				return qerr
			}
			// Both endpoints must be active members of the same live session;
			// anything less is indistinguishable from a stranger probing, so
			// it 404s below rather than leaking the session's existence.
			if active != 2 {
				return errGameSessionForbidden
			}
			signalID, qerr = q.InsertGameSessionSignal(ctx, sqlcgen.InsertGameSessionSignalParams{
				SessionID:     in.ID,
				FromPlayerID:  fromID,
				ToPlayerID:    in.Body.ToPlayerID,
				NegotiationID: in.Body.NegotiationID,
				Kind:          in.Body.Kind,
				Payload:       in.Body.Payload,
			})
			return qerr
		})
		if err != nil {
			// The send did not commit, so restore its token. Use a non-cancelled
			// context because a request deadline may be the failure being undone.
			if refunder, ok := d.Limiter.(ratelimit.Refunder); ok {
				if refundErr := refunder.Refund(context.WithoutCancel(ctx), bucket, signalRatePerSecond, signalBurst); refundErr != nil {
					if metrics.refundErrors != nil {
						metrics.refundErrors.Inc()
					}
					slog.ErrorContext(ctx, "game session signal rate-limit refund", "err", refundErr, "player_id", fromID)
				}
			}
		}
		switch {
		case errors.Is(err, errGameSessionForbidden):
			return nil, huma.Error404NotFound("not found")
		case err != nil:
			return nil, serverError(ctx, "game session signal send", err)
		}
		return &gameSessionSignalSendOutput{Body: gameSessionSignalSendResult{ID: signalID}}, nil
	}
}

func gameSessionSignalPoll(d Deps) func(context.Context, *gameSessionSignalPollInput) (*gameSessionSignalPollOutput, error) {
	return func(ctx context.Context, in *gameSessionSignalPollInput) (*gameSessionSignalPollOutput, error) {
		playerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			return nil, huma.Error400BadRequest("api key has no project pin")
		}
		var rows []sqlcgen.ListGameSessionSignalsForRecipientRow
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			active, qerr := q.CountActiveSignalMembers(ctx, sqlcgen.CountActiveSignalMembersParams{
				SessionID: in.ID,
				ProjectID: projectID,
				PlayerIds: []int64{playerID},
			})
			if qerr != nil {
				return qerr
			}
			if active != 1 {
				return errGameSessionForbidden
			}
			rows, qerr = q.ListGameSessionSignalsForRecipient(ctx, sqlcgen.ListGameSessionSignalsForRecipientParams{
				SessionID:  in.ID,
				ToPlayerID: playerID,
				AfterID:    in.AfterID,
			})
			return qerr
		})
		if errors.Is(err, errGameSessionForbidden) {
			return nil, huma.Error404NotFound("not found")
		}
		if err != nil {
			return nil, serverError(ctx, "game session signal poll", err)
		}
		entries := make([]gameSessionSignalEntry, 0, len(rows))
		for _, r := range rows {
			entries = append(entries, gameSessionSignalEntry{
				ID:            r.ID,
				FromPlayerID:  r.FromPlayerID,
				ToPlayerID:    r.ToPlayerID,
				NegotiationID: r.NegotiationID,
				Kind:          r.Kind,
				Payload:       r.Payload,
				CreatedAt:     r.CreatedAt.Time,
			})
		}
		return &gameSessionSignalPollOutput{Body: gameSessionSignalPollResult{Signals: entries}}, nil
	}
}
