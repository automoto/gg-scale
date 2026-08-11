package jobs

import (
	"time"

	"github.com/riverqueue/river"
)

// PeriodicRegistration pairs a periodic job's args with its recurrence
// interval.
type PeriodicRegistration struct {
	Args     river.JobArgs
	Interval time.Duration
}

// PeriodicRegistrations is the single source of truth for the periodic
// maintenance schedule; the server also runs each job once at startup.
// River does not persist periodic schedules — interval timers reset on every
// process start — so an interval longer than the typical deploy cadence
// degrades to run-on-start only. Keep intervals at or below the TTL of the
// rows a job reaps.
func PeriodicRegistrations() []PeriodicRegistration {
	return []PeriodicRegistration{
		{Args: GameSessionGCArgs{}, Interval: time.Hour},
		{Args: TrustedDeviceGCArgs{}, Interval: 24 * time.Hour},
		{Args: ConnectionGrantGCArgs{}, Interval: time.Hour},
		{Args: MatchmakerGCArgs{}, Interval: time.Hour},
		{Args: StorageWarnArgs{}, Interval: time.Hour},
		{Args: PasswordResetGCArgs{}, Interval: 24 * time.Hour},
		{Args: PlayerDeletePurgeArgs{}, Interval: time.Hour},
		// Short interval: a period reset should land within minutes of its
		// calendar boundary, and the sweep is one indexed query per tenant
		// when nothing is due.
		{Args: LeaderboardPeriodResetArgs{}, Interval: 5 * time.Minute},
	}
}
