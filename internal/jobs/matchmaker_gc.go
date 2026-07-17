package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/fleet"
)

// MatchmakerGCKind is the River job kind for the matchmaker retention sweep.
const MatchmakerGCKind = "matchmaker_gc"

// matchmakerTicketRetention is how long terminal (matched/cancelled/failed)
// tickets are kept for polling and debugging before the sweep drops them.
const matchmakerTicketRetention = 24 * time.Hour

// MatchmakerGCArgs is the (argument-less) periodic GC job. River schedules it
// on the elected leader, so it runs once across the fleet rather than once
// per instance.
type MatchmakerGCArgs struct{}

// Kind implements river.JobArgs.
func (MatchmakerGCArgs) Kind() string { return MatchmakerGCKind }

// MatchmakerGCWorker deletes expired matchmaker_matches rows and terminal
// tickets past retention.
type MatchmakerGCWorker struct {
	river.WorkerDefaults[MatchmakerGCArgs]
	pool     *db.Pool
	releaser MatchmakerAllocationReleaser
}

// MatchmakerAllocationReleaser is the fleet manager surface used by GC.
type MatchmakerAllocationReleaser interface {
	Deallocate(ctx context.Context, id fleet.AllocationID) error
}

// NewMatchmakerGCWorker returns a worker bound to the app pool and fleet
// allocation releaser. A nil releaser preserves candidates for a later retry.
func NewMatchmakerGCWorker(pool *db.Pool, releaser MatchmakerAllocationReleaser) *MatchmakerGCWorker {
	return &MatchmakerGCWorker{pool: pool, releaser: releaser}
}

// Work implements river.Worker by sweeping matchmaker retention.
func (w *MatchmakerGCWorker) Work(ctx context.Context, _ *river.Job[MatchmakerGCArgs]) error {
	return SweepMatchmakerRecords(ctx, w.pool, w.releaser)
}

// SweepMatchmakerRecords releases expired unclaimed fleet allocations, then
// drops expired match rows and terminal tickets. Backend cleanup happens
// outside a database transaction; match deletion follows only on success so
// River retries preserve reconciliation state.
func SweepMatchmakerRecords(ctx context.Context, pool *db.Pool, releaser MatchmakerAllocationReleaser) error {
	var candidates []sqlcgen.ListExpiredUnclaimedMatchmakerAllocationsRow
	if err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var err error
		candidates, err = sqlcgen.New(tx).ListExpiredUnclaimedMatchmakerAllocations(ctx)
		return err
	}); err != nil {
		return err
	}

	var sweepErrors []error
	released := int64(0)
	if releaser == nil && len(candidates) > 0 {
		sweepErrors = append(sweepErrors, fmt.Errorf("matchmaker GC: no allocation releaser for %d expired match(es)", len(candidates)))
	} else {
		for _, candidate := range candidates {
			releaseCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			releaseCtx = db.WithTenant(releaseCtx, candidate.TenantID)
			err := releaser.Deallocate(releaseCtx, fleet.AllocationID(candidate.AllocationID))
			cancel()
			if err != nil {
				sweepErrors = append(sweepErrors, fmt.Errorf("deallocate match %s allocation %d: %w", candidate.ID, candidate.AllocationID, err))
				continue
			}
			err = pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
				_, err := sqlcgen.New(tx).DeleteExpiredUnclaimedMatchmakerMatch(ctx, candidate.ID)
				return err
			})
			if err != nil {
				sweepErrors = append(sweepErrors, fmt.Errorf("delete match %s after deallocation: %w", candidate.ID, err))
				continue
			}
			released++
		}
	}

	err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		matches, err := q.DeleteExpiredMatchmakerMatches(ctx)
		if err != nil {
			return err
		}
		tickets, err := q.DeleteTerminalMatchmakerTickets(ctx, pgtype.Interval{
			Microseconds: matchmakerTicketRetention.Microseconds(),
			Valid:        true,
		})
		if err != nil {
			return err
		}
		if released > 0 || matches > 0 || tickets > 0 {
			slog.InfoContext(ctx, "matchmaker GC", "allocations_released", released,
				"matches_deleted", matches, "tickets_deleted", tickets)
		}
		return nil
	})
	if err != nil {
		sweepErrors = append(sweepErrors, err)
	}
	return errors.Join(sweepErrors...)
}
