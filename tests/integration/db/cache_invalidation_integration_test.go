//go:build integration

package db_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/cacheinvalidation"
	"github.com/automoto/gg-scale/internal/db"
)

type invalidationEvent struct {
	Kind     string `json:"kind"`
	TenantID int64  `json:"tenant_id"`
	PlayerID int64  `json:"player_id"`
}

func TestCacheInvalidation_should_deliver_committed_writes(t *testing.T) {
	pool := startMigrated(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var tenantID, projectID, playerID, keyID int64
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO tenants (name) VALUES ('cache-notify') RETURNING id`).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'cache-notify') RETURNING id`, tenantID).Scan(&projectID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id) VALUES ($1, $2, 'cache-notify') RETURNING id`,
		tenantID, projectID).Scan(&playerID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO api_keys (tenant_id, project_id, key_hash, key_type) VALUES ($1, $2, '\x01', 'secret') RETURNING id`,
		tenantID, projectID).Scan(&keyID))
	_, err := pool.Exec(ctx,
		`INSERT INTO rate_limit_overrides (tenant_id, kind, rate, burst) VALUES ($1, 'api', 10, 20)`, tenantID)
	require.NoError(t, err)

	ready := make(chan struct{})
	messages := make(chan string, 8)
	listenDone := make(chan error, 1)
	go func() {
		listenDone <- db.NewPool(pool).ListenChannelReady(ctx, cacheinvalidation.Channel, func() { close(ready) }, func(payload string) {
			messages <- payload
		})
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("cache invalidation listener did not become ready")
	}

	started := time.Now()
	_, err = pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, keyID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE rate_limit_overrides SET rate = 11 WHERE tenant_id = $1`, tenantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE project_players SET session_epoch = session_epoch + 1 WHERE id = $1`, playerID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM project_players WHERE id = $1`, playerID)
	require.NoError(t, err)

	got := make([]invalidationEvent, 0, 4)
	for len(got) < 4 {
		select {
		case payload := <-messages:
			var event invalidationEvent
			require.NoError(t, json.Unmarshal([]byte(payload), &event))
			got = append(got, event)
		case <-time.After(time.Second):
			t.Fatalf("received %d of 4 invalidations within one second", len(got))
		}
	}
	assert.Less(t, time.Since(started), time.Second)
	assert.Equal(t, []invalidationEvent{
		{Kind: "api_key", TenantID: tenantID},
		{Kind: "override", TenantID: tenantID},
		{Kind: "session_epoch", PlayerID: playerID},
		{Kind: "session_epoch", PlayerID: playerID},
	}, got)

	cancel()
	require.NoError(t, <-listenDone)
}
