package httpapi

// Lifecycle sweeping for live WebSockets. The per-connection heartbeat check
// is pure in-memory (an idle socket never touches Postgres — see
// docs/capacity-and-launch.md); a single per-process sweeper batches the
// database work into one small query per tenant with open sockets per
// interval, O(tenants) rather than O(sockets). Definitive answers (revoked
// epoch, disabled/missing tenant, deleted player) mark the connection stale
// and the next heartbeat closes it. Infrastructure errors do NOT fail open
// indefinitely: they stop advancing the connection's last-verified stamp, and
// after a bounded grace the connection closes as unverifiable.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/playerauth"
)

var (
	errRealtimeSessionRevoked = errors.New("realtime: session revoked")
	errRealtimeUnverifiable   = errors.New("realtime: lifecycle unverifiable past grace")
)

// wsGraceIntervals bounds how many sweep intervals a connection may go
// without a successful lifecycle verification before it is closed.
const wsGraceIntervals = 4

// wsConnState is one live connection's lifecycle view. The sweeper writes,
// the connection's heartbeat reads; both sides stay lock-free via the
// owning wsLifecycle mutex (sweeper) and value copies (check).
type wsConnState struct {
	tenantID   int64
	playerID   int64
	claimEpoch int64

	mu           sync.Mutex
	stale        bool
	lastVerified time.Time
}

func (s *wsConnState) markStale() {
	s.mu.Lock()
	s.stale = true
	s.mu.Unlock()
}

func (s *wsConnState) markVerified(now time.Time) {
	s.mu.Lock()
	if !s.stale {
		s.lastVerified = now
	}
	s.mu.Unlock()
}

// check is the per-heartbeat revalidation hook: no I/O, just the sweeper's
// latest verdict plus the bounded-grace clock.
func (s *wsConnState) check(grace time.Duration) error {
	s.mu.Lock()
	stale, lastVerified := s.stale, s.lastVerified
	s.mu.Unlock()
	if stale {
		return errRealtimeSessionRevoked
	}
	if time.Since(lastVerified) > grace {
		return errRealtimeUnverifiable
	}
	return nil
}

// wsLifecycle tracks live connections and runs the batched sweep. The sweep
// goroutine starts with the first connection and exits when none remain.
type wsLifecycle struct {
	pool     *db.Pool
	interval time.Duration
	metrics  *observability.Metrics

	mu      sync.Mutex
	conns   map[*wsConnState]struct{}
	running bool
}

func newWSLifecycle(pool *db.Pool, interval time.Duration, metrics *observability.Metrics) *wsLifecycle {
	return &wsLifecycle{
		pool:     pool,
		interval: interval,
		metrics:  metrics,
		conns:    map[*wsConnState]struct{}{},
	}
}

func (l *wsLifecycle) grace() time.Duration {
	return wsGraceIntervals * l.interval
}

// register adds the request's connection to the sweep set. ok is false when
// the request context lacks the identities (the WS handler rejects those
// requests itself).
func (l *wsLifecycle) register(ctx context.Context) (*wsConnState, bool) {
	tenantID, err := db.TenantFromContext(ctx)
	if err != nil {
		return nil, false
	}
	playerID, okPlayer := playerauth.IDFromContext(ctx)
	claimEpoch, okEpoch := playerauth.SessionEpochFromContext(ctx)
	if !okPlayer || !okEpoch {
		return nil, false
	}
	st := &wsConnState{
		tenantID:     tenantID,
		playerID:     playerID,
		claimEpoch:   claimEpoch,
		lastVerified: time.Now(), // the handshake just validated everything
	}
	l.mu.Lock()
	l.conns[st] = struct{}{}
	if !l.running {
		l.running = true
		go l.run()
	}
	l.mu.Unlock()
	return st, true
}

func (l *wsLifecycle) unregister(st *wsConnState) {
	l.mu.Lock()
	delete(l.conns, st)
	l.mu.Unlock()
}

// run sweeps until no connections remain, then exits (a later register
// starts a fresh goroutine). Keeps idle processes goroutine-free.
func (l *wsLifecycle) run() {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for range ticker.C {
		l.mu.Lock()
		if len(l.conns) == 0 {
			l.running = false
			l.mu.Unlock()
			return
		}
		byTenant := map[int64][]*wsConnState{}
		for st := range l.conns {
			byTenant[st.tenantID] = append(byTenant[st.tenantID], st)
		}
		l.mu.Unlock()

		for tenantID, conns := range byTenant {
			l.sweepTenant(tenantID, conns)
		}
	}
}

// sweepTenant verifies one tenant's connections in a single transaction.
// On an infrastructure error nothing is marked verified — the bounded grace
// in check() closes the tenant's sockets if the state stays unverifiable.
func (l *wsLifecycle) sweepTenant(tenantID int64, conns []*wsConnState) {
	ctx, cancel := context.WithTimeout(context.Background(), l.interval)
	defer cancel()
	ctx = db.WithTenant(ctx, tenantID)

	players := make([]int64, 0, len(conns))
	seen := map[int64]struct{}{}
	for _, st := range conns {
		if _, ok := seen[st.playerID]; ok {
			continue
		}
		seen[st.playerID] = struct{}{}
		players = append(players, st.playerID)
	}

	var (
		tenantGone bool
		disabled   bool
		epochs     = make(map[int64]int32, len(players))
	)
	err := l.pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		state, serr := q.GetTenantDisabledState(ctx, tenantID)
		if errors.Is(serr, pgx.ErrNoRows) {
			tenantGone = true
			return nil
		}
		if serr != nil {
			return serr
		}
		disabled = state.DisabledAt.Valid
		rows, serr := q.ListPlayerSessionEpochs(ctx, players)
		if serr != nil {
			return serr
		}
		for _, row := range rows {
			epochs[row.ID] = row.SessionEpoch
		}
		return nil
	})
	if err != nil {
		// Infrastructure failure: lastVerified stalls and the bounded grace
		// closes the tenant's sockets if this persists. Already aggregated —
		// one log line + counter per tenant per interval, never per socket —
		// so a schema/permission regression is visible before the closes hit.
		l.metrics.RealtimeSweepFailure()
		slog.Warn("realtime: lifecycle sweep failed; sockets close after bounded grace if this persists",
			"err", err, "tenant_id", tenantID, "connections", len(conns), "grace", l.grace())
		return
	}

	now := time.Now()
	for _, st := range conns {
		if tenantGone || disabled {
			st.markStale()
			continue
		}
		epoch, ok := epochs[st.playerID]
		if !ok || int64(epoch) != st.claimEpoch {
			st.markStale()
			continue
		}
		st.markVerified(now)
	}
}
