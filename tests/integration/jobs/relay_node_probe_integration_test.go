//go:build integration

package jobs_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/jobs"
	"github.com/automoto/gg-scale/internal/relaynode"
)

func TestRelayNodeProbeWorker_should_record_probe_results_without_removing_rows(t *testing.T) {
	_, raw := startJobsDB(t)
	ctx := context.Background()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"role":"relay","state":"active","active_work":4}`)
	}))
	t.Cleanup(up.Close)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	store := relaynode.NewStore(appRolePool(t, raw))
	_, err := store.Create(ctx, relaynode.Spec{Region: "us-east", TURNURLs: []string{"turn:up.example.com:3478"}, LifecycleURL: up.URL})
	require.NoError(t, err)
	_, err = store.Create(ctx, relaynode.Spec{Region: "us-east", TURNURLs: []string{"turn:down.example.com:3478"}, LifecycleURL: down.URL})
	require.NoError(t, err)
	monitor := relaynode.NewMonitor(store, relaynode.NewProber("", time.Second), slog.New(slog.DiscardHandler))
	worker := jobs.NewRelayNodeProbeWorker(monitor)

	require.NoError(t, worker.Work(ctx, &river.Job[jobs.RelayNodeProbeArgs]{}))

	nodes, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, nodes, 2)
	assert.Equal(t, []bool{true, false}, []bool{nodes[0].Eligible, nodes[1].Eligible})
}
