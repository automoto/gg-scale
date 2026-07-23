//go:build integration

package matchmaker_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/matchmaker"
	"github.com/ggscale/ggscale/internal/observability"
)

// seedTenantProjectPlayer creates a fresh tenant + project + one player,
// returning their ids. namePrefix keeps rows readable in a shared container.
func seedTenantProjectPlayer(t *testing.T, pool *pgxpool.Pool, namePrefix, externalID string) (tenantID, projectID, playerID int64) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ($1) RETURNING id`, namePrefix).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p') RETURNING id`, tenantID).Scan(&projectID))
	_, _, playerID = seedTenantProjectPlayerInto(t, pool, tenantID, projectID, externalID)
	return tenantID, projectID, playerID
}

// seedTenantProjectPlayerInto adds one player to an existing tenant/project.
func seedTenantProjectPlayerInto(t *testing.T, pool *pgxpool.Pool, tenantID, projectID int64, externalID string) (int64, int64, int64) {
	t.Helper()
	var playerID int64
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, $3) RETURNING id`, tenantID, projectID, externalID).Scan(&playerID))
	return tenantID, projectID, playerID
}

// mmObserver / mmGauge adapt *observability.Metrics to the worker's optional
// interfaces, mirroring the production adapters in cmd/ggscale-server.
type mmObserver struct{ m *observability.Metrics }

func (o mmObserver) Observe(v float64) { o.m.MatchmakerTimeToMatch(v) }

type mmGauge struct{ m *observability.Metrics }

func (g mmGauge) SetQueueStats(stats []matchmaker.BucketStat) {
	samples := make([]observability.MatchmakerBucketSample, len(stats))
	for i, s := range stats {
		samples[i] = observability.MatchmakerBucketSample{
			Mode: s.Mode, Region: s.Region,
			Depth: float64(s.Depth), OldestAgeSeconds: s.OldestAgeSeconds,
		}
	}
	g.m.SetMatchmakerQueueStats(samples)
}

// TestPGQueueFailureCounterByReason drives one ticket to 'expired' (TTL sweep)
// and one to 'attempts_exhausted' (release at the attempt cap) and asserts the
// ggscale_matchmaker_ticket_failures_total counter increments per reason.
func TestPGQueueFailureCounterByReason(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	ctx := context.Background()

	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	queue := matchmaker.NewPGQueue(appPool).WithFailureRecorder(metrics)

	tenantID, projectID, expPlayer := seedTenantProjectPlayer(t, pool, "mm-fail", "fail-exp")
	_, _, capPlayer := seedTenantProjectPlayerInto(t, pool, tenantID, projectID, "fail-cap")
	tenantCtx := db.WithTenant(ctx, tenantID)

	// Reason "expired": a past-TTL ticket, swept.
	past := time.Now().UTC().Add(-time.Minute)
	_, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, PlayerID: expPlayer,
		Mode: matchmaker.ModeMatchOnly, GameMode: "1v1", ExpiresAt: &past,
	})
	require.NoError(t, err)
	_, err = queue.SweepStaleClaims(ctx, 3)
	require.NoError(t, err)

	// Reason "attempts_exhausted": claim then release at cap=1.
	cap1, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, PlayerID: capPlayer,
		Mode: matchmaker.ModeMatchOnly, GameMode: "1v1",
	})
	require.NoError(t, err)
	bucket := matchmaker.Bucket{TenantID: tenantID, ProjectID: projectID, Mode: matchmaker.ModeMatchOnly, GameMode: "1v1"}
	claim, err := queue.ClaimBucket(ctx, bucket, 10, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.NoError(t, queue.ReleaseTickets(ctx, claim, []int64{cap1.ID}, 1))

	expected := `
# HELP ggscale_matchmaker_ticket_failures_total Matchmaker tickets that ended in 'failed', by reason (expired / attempts_exhausted).
# TYPE ggscale_matchmaker_ticket_failures_total counter
ggscale_matchmaker_ticket_failures_total{reason="attempts_exhausted"} 1
ggscale_matchmaker_ticket_failures_total{reason="expired"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected), "ggscale_matchmaker_ticket_failures_total"))
}

// TestPGQueueTimeToMatchHistogramObserved commits a match and asserts the
// time-to-match histogram recorded one observation per committed ticket.
func TestPGQueueTimeToMatchHistogramObserved(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	ctx := context.Background()

	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	queue := matchmaker.NewPGQueue(appPool)

	tenantID, projectID, playerID := seedTenantProjectPlayer(t, pool, "mm-ttm", "ttm-p1")
	w := matchmaker.NewWorker(queue, nil, nil, matchmaker.WorkerConfig{TimeToMatch: mmObserver{metrics}})

	tenantCtx := db.WithTenant(ctx, tenantID)
	_, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, PlayerID: playerID,
		Mode: matchmaker.ModeMatchOnly, GameMode: "1v1",
	})
	require.NoError(t, err)

	require.NoError(t, w.Tick(ctx))

	h := findHistogram(t, reg, "ggscale_matchmaker_time_to_match_seconds")
	require.NotNil(t, h)
	assert.Equal(t, uint64(1), h.GetSampleCount())
	assert.GreaterOrEqual(t, h.GetSampleSum(), 0.0)
}

// TestPGQueueQueueDepthAndOldestAgeGauges seeds queued tickets that differ only
// by game_mode and asserts one collection pass folds them into a single
// (mode, region) gauge series (game_mode is deliberately not a label) with the
// summed depth and a non-zero oldest age.
func TestPGQueueQueueDepthAndOldestAgeGauges(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	ctx := context.Background()

	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	queue := matchmaker.NewPGQueue(appPool)

	tenantID, projectID, p1 := seedTenantProjectPlayer(t, pool, "mm-depth", "depth-p1")
	_, _, p2 := seedTenantProjectPlayerInto(t, pool, tenantID, projectID, "depth-p2")
	_, _, p3 := seedTenantProjectPlayerInto(t, pool, tenantID, projectID, "depth-p3")
	tenantCtx := db.WithTenant(ctx, tenantID)

	// Two match_only "1v1" tickets and one "coop" ticket: all the same
	// (mode=match_only, region="") gauge bucket once game_mode is dropped.
	for _, pid := range []int64{p1, p2} {
		_, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
			TenantID: tenantID, ProjectID: projectID, PlayerID: pid,
			Mode: matchmaker.ModeMatchOnly, GameMode: "1v1",
		})
		require.NoError(t, err)
	}
	_, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, PlayerID: p3,
		Mode: matchmaker.ModeMatchOnly, GameMode: "coop",
	})
	require.NoError(t, err)

	// Let the oldest ticket age a touch so the gauge is provably non-zero.
	time.Sleep(1100 * time.Millisecond)

	w := matchmaker.NewWorker(queue, nil, nil, matchmaker.WorkerConfig{QueueGauge: mmGauge{metrics}})
	require.NoError(t, w.CollectStats(ctx))

	expectedDepth := `
# HELP ggscale_matchmaker_queue_depth Queued, unclaimed matchmaker tickets per bucket, sampled on the sweep cadence.
# TYPE ggscale_matchmaker_queue_depth gauge
ggscale_matchmaker_queue_depth{mode="match_only",region=""} 3
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expectedDepth), "ggscale_matchmaker_queue_depth"))

	age := gaugeValue(t, reg, "ggscale_matchmaker_oldest_ticket_age_seconds", map[string]string{
		"mode": "match_only", "region": "",
	})
	assert.Greater(t, age, 1.0, "oldest ticket in the bucket has aged at least ~1s")
}

// --- helpers ---

func findHistogram(t *testing.T, reg *prometheus.Registry, name string) *dto.Histogram {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		require.Len(t, mf.GetMetric(), 1)
		return mf.GetMetric()[0].GetHistogram()
	}
	return nil
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, mtr := range mf.GetMetric() {
			if labelsMatch(mtr, labels) {
				return mtr.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("gauge %s with labels %v not found", name, labels)
	return 0
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		got[l.GetName()] = l.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
