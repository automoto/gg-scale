package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/matchmaker"
	"github.com/automoto/gg-scale/internal/observability"
	"github.com/automoto/gg-scale/internal/relaynode"
)

func humaStatus(t *testing.T, err error) int {
	t.Helper()
	require.Error(t, err)
	se, ok := err.(huma.StatusError)
	require.True(t, ok, "error is not a huma.StatusError: %v", err)
	return se.GetStatus()
}

func TestVerifyRelayMatchMembership(t *testing.T) {
	q := matchmaker.NewMemQueue()
	tctx := db.WithTenant(context.Background(), 1)
	require.NoError(t, q.CreateMatch(tctx, &matchmaker.Match{
		ID:        "mm_room",
		TenantID:  1,
		ProjectID: 7,
		Roster:    []matchmaker.RosterEntry{{PlayerID: 41}, {PlayerID: 42}},
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}))
	d := Deps{Matchmaker: q}

	t.Run("member is allowed", func(t *testing.T) {
		assert.NoError(t, verifyRelayMatchMembership(tctx, d, 7, 42, "mm_room"))
	})
	t.Run("non-member is forbidden", func(t *testing.T) {
		assert.Equal(t, 403, humaStatus(t, verifyRelayMatchMembership(tctx, d, 7, 99, "mm_room")))
	})
	t.Run("cross-project match is forbidden", func(t *testing.T) {
		assert.Equal(t, 403, humaStatus(t, verifyRelayMatchMembership(tctx, d, 8, 41, "mm_room")))
	})
	t.Run("unknown match is forbidden, not enumerable", func(t *testing.T) {
		assert.Equal(t, 403, humaStatus(t, verifyRelayMatchMembership(tctx, d, 7, 41, "mm_missing")))
	})
	t.Run("no matchmaker configured", func(t *testing.T) {
		assert.Equal(t, 400, humaStatus(t, verifyRelayMatchMembership(tctx, Deps{}, 7, 41, "mm_room")))
	})
}

type stubRelayNodes struct {
	sel relaynode.Selection
	err error
}

func (s stubRelayNodes) Select(context.Context, string) (relaynode.Selection, error) {
	return s.sel, s.err
}

func TestSelectRelayNodes_should_return_503_without_a_selector(t *testing.T) {
	_, err := selectRelayNodes(t.Context(), Deps{}, "us-east")

	assert.Equal(t, http.StatusServiceUnavailable, wrappedHumaStatus(t, err))
}

func TestSelectRelayNodes_should_return_503_when_no_node_is_eligible(t *testing.T) {
	d := Deps{RelayNodes: stubRelayNodes{err: relaynode.ErrNoEligibleNodes}}

	_, err := selectRelayNodes(t.Context(), d, "us-east")

	assert.Equal(t, http.StatusServiceUnavailable, wrappedHumaStatus(t, err))
}

// wrappedHumaStatus reads the status of a huma error that may be wrapped,
// for example by huma.ErrorWithHeaders.
func wrappedHumaStatus(t *testing.T, err error) int {
	t.Helper()
	var se huma.StatusError
	require.ErrorAs(t, err, &se)
	return se.GetStatus()
}

func TestSelectRelayNodes_should_set_retry_after_on_503(t *testing.T) {
	d := Deps{RelayNodes: stubRelayNodes{err: relaynode.ErrNoEligibleNodes}}

	_, err := selectRelayNodes(t.Context(), d, "us-east")

	var he huma.HeadersError
	require.ErrorAs(t, err, &he)
	assert.Equal(t, "10", he.GetHeaders().Get("Retry-After"))
}

func TestSelectRelayNodes_should_count_a_refusal_in_the_metric(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	d := Deps{RelayNodes: stubRelayNodes{err: relaynode.ErrNoEligibleNodes}, Metrics: metrics}

	_, _ = selectRelayNodes(t.Context(), d, "us-east")

	assert.InDelta(t, 1, counterValue(t, reg, "ggscale_relay_no_eligible_nodes_total"), 0)
}

func counterValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() == name {
			return f.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatalf("metric %s not registered", name)
	return 0
}

func TestSelectRelayNodes_should_return_500_on_a_store_error(t *testing.T) {
	d := Deps{RelayNodes: stubRelayNodes{err: errors.New("db down")}}

	_, err := selectRelayNodes(t.Context(), d, "us-east")

	assert.Equal(t, http.StatusInternalServerError, humaStatus(t, err))
}

func TestSelectRelayNodes_should_return_the_selection(t *testing.T) {
	want := relaynode.Selection{URLs: []string{"turn:a:3478"}, Region: "us-east"}
	d := Deps{RelayNodes: stubRelayNodes{sel: want}}

	got, err := selectRelayNodes(t.Context(), d, "us-east")

	require.NoError(t, err)
	assert.Equal(t, want, got)
}
