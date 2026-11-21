//go:build integration

package projectadmin_test

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/projectadmin"
)

func (f fixture) setLeaderboardLimit(t *testing.T, limit int64) {
	t.Helper()
	_, err := f.owner.Exec(context.Background(),
		`INSERT INTO tenant_quota_overrides (tenant_id, axis, "limit") VALUES ($1, 'leaderboards', $2)`, f.tenantID, limit)
	require.NoError(t, err)
}

func TestLeaderboardLimit_create_should_fail_at_limit(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.setLeaderboardLimit(t, 1)
	f.createBoard(t, "first")

	// Act
	_, err := projectadmin.CreateLeaderboard(context.Background(), f.pool, f.tenantID, f.project, board("second"), f.user)

	// Assert
	assert.ErrorIs(t, err, projectadmin.ErrLeaderboardLimit)
}

func TestLeaderboardLimit_should_not_count_deleted_boards(t *testing.T) {
	// Arrange
	f := newFixture(t)
	ctx := context.Background()
	f.setLeaderboardLimit(t, 1)
	id := f.createBoard(t, "first")
	require.NoError(t, projectadmin.DeleteLeaderboard(ctx, f.pool, f.tenantID, f.project, id, f.user))

	// Act
	_, err := projectadmin.CreateLeaderboard(ctx, f.pool, f.tenantID, f.project, board("second"), f.user)

	// Assert
	assert.NoError(t, err)
}

func TestLeaderboardLimit_restore_should_fail_at_limit(t *testing.T) {
	// Arrange
	f := newFixture(t)
	ctx := context.Background()
	f.setLeaderboardLimit(t, 1)
	id := f.createBoard(t, "old")
	require.NoError(t, projectadmin.DeleteLeaderboard(ctx, f.pool, f.tenantID, f.project, id, f.user))
	f.createBoard(t, "new")

	// Act
	err := projectadmin.RestoreLeaderboard(ctx, f.pool, f.tenantID, f.project, id, f.user)

	// Assert
	assert.ErrorIs(t, err, projectadmin.ErrLeaderboardLimit)
}

func TestLeaderboardLimit_should_apply_when_quotas_are_not_enforced(t *testing.T) {
	// Arrange
	f := newFixture(t)
	_, err := f.owner.Exec(context.Background(), `UPDATE tenants SET enforce_quotas = false WHERE id = $1`, f.tenantID)
	require.NoError(t, err)
	f.setLeaderboardLimit(t, 1)
	f.createBoard(t, "first")

	// Act
	_, err = projectadmin.CreateLeaderboard(context.Background(), f.pool, f.tenantID, f.project, board("second"), f.user)

	// Assert
	assert.ErrorIs(t, err, projectadmin.ErrLeaderboardLimit)
}

func TestLeaderboardLimit_concurrent_creates_at_one_below_limit_should_give_one_success(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.setLeaderboardLimit(t, 2)
	f.createBoard(t, "first")
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	errs := make([]error, len(names))
	start := make(chan struct{})
	var wg sync.WaitGroup

	// Act
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = projectadmin.CreateLeaderboard(context.Background(), f.pool, f.tenantID, f.project, board(name), f.user)
		}()
	}
	close(start)
	wg.Wait()

	// Assert
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		require.ErrorIs(t, err, projectadmin.ErrLeaderboardLimit)
	}
	assert.Equal(t, 1, ok)
}

func TestLeaderboardLimit_restore_of_live_board_at_limit_should_not_find_it(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.setLeaderboardLimit(t, 1)
	id := f.createBoard(t, "live")

	// Act
	err := projectadmin.RestoreLeaderboard(context.Background(), f.pool, f.tenantID, f.project, id, f.user)

	// Assert
	assert.ErrorIs(t, err, pgx.ErrNoRows)
}

func TestLeaderboardLimit_restore_below_limit_should_succeed(t *testing.T) {
	// Arrange
	f := newFixture(t)
	ctx := context.Background()
	f.setLeaderboardLimit(t, 1)
	id := f.createBoard(t, "old")
	require.NoError(t, projectadmin.DeleteLeaderboard(ctx, f.pool, f.tenantID, f.project, id, f.user))

	// Act
	err := projectadmin.RestoreLeaderboard(ctx, f.pool, f.tenantID, f.project, id, f.user)

	// Assert
	assert.NoError(t, err)
}
