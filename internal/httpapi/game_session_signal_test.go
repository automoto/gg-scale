package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/playerauth"
	"github.com/automoto/gg-scale/internal/ratelimit"
)

type signalDenyLimiter struct {
	calls int
}

func (l *signalDenyLimiter) Allow(context.Context, string, float64, float64) (ratelimit.Decision, error) {
	l.calls++
	return ratelimit.Decision{Allowed: false, RetryAfter: 1500 * time.Millisecond}, nil
}

func TestGameSessionSignalSend_should_throttle_before_any_db_work(t *testing.T) {
	limiter := &signalDenyLimiter{}
	handler := gameSessionSignalSend(Deps{Limiter: limiter})
	ctx := playerauth.WithID(db.WithProject(context.Background(), 9), 42)
	in := &gameSessionSignalSendInput{ID: "gs_test", Body: gameSessionSignalRequest{
		ToPlayerID: 43, NegotiationID: "neg-1", Kind: "offer", Payload: "v=0",
	}}

	out, err := handler(ctx, in)

	assert.Nil(t, out)
	require.Error(t, err)
	var status huma.StatusError
	require.ErrorAs(t, err, &status)
	assert.Equal(t, 429, status.GetStatus())
	var withHeaders huma.HeadersError
	require.ErrorAs(t, err, &withHeaders)
	assert.Equal(t, "2", withHeaders.GetHeaders().Get("Retry-After"))
	assert.Equal(t, 1, limiter.calls)
}

func TestSignalRateLimited_should_round_retry_after_up(t *testing.T) {
	tests := []struct {
		name  string
		retry time.Duration
		want  string
	}{
		{name: "minimum", want: "1"},
		{name: "fraction", retry: time.Nanosecond, want: "1"},
		{name: "round up", retry: 1200 * time.Millisecond, want: "2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := signalRateLimited(tt.retry)
			var withHeaders huma.HeadersError
			require.ErrorAs(t, err, &withHeaders)

			got, convErr := strconv.Atoi(withHeaders.GetHeaders().Get("Retry-After"))
			require.NoError(t, convErr)
			assert.Equal(t, tt.want, strconv.Itoa(got))
			var status huma.StatusError
			require.ErrorAs(t, err, &status)
			assert.Equal(t, http.StatusTooManyRequests, status.GetStatus())
		})
	}
}

// The recipient, negotiation_id, and kind constraints are declared as huma
// struct tags on gameSessionSignalRequest and enforced by the schema layer
// (covered by the integration tests). Only the payload byte cap — which JSON
// Schema's rune-counting maxLength cannot express — is validated in Go, so
// that is what these unit tests exercise.
func TestValidateSignalPayloadSize_rejectsEmpty(t *testing.T) {
	assert.Error(t, validateSignalPayloadSize(""))
}

func TestValidateSignalPayloadSize_rejectsOversized(t *testing.T) {
	assert.Error(t, validateSignalPayloadSize(strings.Repeat("x", maxGameSessionSignalBytes+1)))
}

func TestValidateSignalPayloadSize_acceptsWithinBounds(t *testing.T) {
	assert.NoError(t, validateSignalPayloadSize("v=0"))
	assert.NoError(t, validateSignalPayloadSize(strings.Repeat("x", maxGameSessionSignalBytes)))
}
