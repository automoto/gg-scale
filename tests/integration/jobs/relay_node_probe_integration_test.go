//go:build integration

package jobs_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// syncBuffer is a log sink that several monitors share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Several API servers probe in-process when River is down. The row lock in
// RecordRelayNodeProbe hands each stored result to one writer, so the
// mismatch is logged once in total, not once per server.
func TestRelayNodeMonitors_should_log_a_token_mismatch_once_across_servers(t *testing.T) {
	_, raw := startJobsDB(t)
	ctx := context.Background()
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(node.Close)
	store := relaynode.NewStore(appRolePool(t, raw))
	_, err := store.Create(ctx, relaynode.Spec{Region: "us-east", TURNURLs: []string{"turn:a.example.com:3478"}, LifecycleURL: node.URL})
	require.NoError(t, err)
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	var wg sync.WaitGroup

	for range 16 {
		m := relaynode.NewMonitor(store, relaynode.NewProber("", time.Second), logger)
		wg.Go(func() { assert.NoError(t, m.ProbeAll(ctx)) })
	}
	wg.Wait()

	assert.Equal(t, 1, strings.Count(logs.String(), "RELAY_PROBE_TOKEN"))
}
