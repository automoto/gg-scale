package jobs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ggscale/ggscale/internal/gamesession"
)

func intervalFor(t *testing.T, kind string) time.Duration {
	t.Helper()
	for _, r := range PeriodicRegistrations() {
		if r.Args.Kind() == kind {
			return r.Interval
		}
	}
	t.Fatalf("no periodic registration for kind %q", kind)
	return 0
}

func TestPeriodicRegistrations_game_session_gc_runs_hourly(t *testing.T) {
	assert.Equal(t, time.Hour, intervalFor(t, GameSessionGCKind))
}

// Sessions expire after gamesession.DefaultTTL; the GC must recur at least
// that often or expired rows pile up between deploys.
func TestPeriodicRegistrations_game_session_gc_keeps_pace_with_session_ttl(t *testing.T) {
	assert.LessOrEqual(t, intervalFor(t, GameSessionGCKind), gamesession.DefaultTTL)
}

func TestPeriodicRegistrations_all_jobs_registered_with_positive_interval(t *testing.T) {
	kinds := make(map[string]bool)
	for _, r := range PeriodicRegistrations() {
		kinds[r.Args.Kind()] = true
		assert.Positive(t, r.Interval, "kind %s", r.Args.Kind())
	}
	for _, want := range []string{
		GameSessionGCKind, TrustedDeviceGCKind, ConnectionGrantGCKind,
		MatchmakerGCKind, StorageWarnKind,
	} {
		assert.True(t, kinds[want], "missing periodic registration for %s", want)
	}
}
