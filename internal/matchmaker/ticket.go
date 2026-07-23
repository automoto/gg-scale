// Package matchmaker turns player "find me a match" requests into match
// results. Players POST tickets through HTTP; a background worker batches
// them by bucket (tenant, project, mode, fleet, region, game_mode), stakes a
// claim on the bucket, resolves the result for the ticket's mode —
// match_only groups players, game_session creates a session, and
// fleet_allocation calls fleet.Manager.Allocate — then commits the claim,
// flipping the rows to 'matched' and pushing a matched envelope back to each
// player over the realtime hub.
//
// The two-phase claim (Claim → Allocate → Commit/Release) is the core
// correctness contract: rows stay 'queued' while the worker is in flight,
// so a worker crash between Allocate and Commit leaves a sweeper-recoverable
// claim instead of a stranded ticket.
package matchmaker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ggscale/ggscale/internal/fleet"
)

// Status is the lifecycle position of a ticket.
type Status string

// Ticket lifecycle values. They mirror the matchmaking_tickets.ticket_status
// Postgres enum.
const (
	StatusQueued    Status = "queued"
	StatusMatched   Status = "matched"
	StatusCancelled Status = "cancelled"
	StatusFailed    Status = "failed"
)

// Mode selects what a matched ticket resolves to: a bare roster
// (match_only), a created game session (game_session), or a dedicated
// server allocation (fleet_allocation).
type Mode string

// Mode values mirror the CHECK constraint on matchmaking_tickets.mode.
const (
	ModeMatchOnly       Mode = "match_only"
	ModeGameSession     Mode = "game_session"
	ModeFleetAllocation Mode = "fleet_allocation"
)

// Failure reasons stamped on a ticket when it flips to StatusFailed. They
// mirror the CHECK on matchmaking_tickets.failure_reason and are surfaced in
// the ticket poll response. Documented as an open enum: more values may be
// added by forward migration.
const (
	failureReasonExpired           = "expired"
	failureReasonAttemptsExhausted = "attempts_exhausted"
)

// ValidMode reports whether m is a known ticket mode.
func ValidMode(m Mode) bool {
	switch m {
	case ModeMatchOnly, ModeGameSession, ModeFleetAllocation:
		return true
	}
	return false
}

// Ticket is one in-flight matchmaking request.
type Ticket struct {
	ID                int64
	TenantID          int64
	ProjectID         int64
	FleetID           int64
	PlayerID          int64
	Mode              Mode
	Region            string
	GameMode          string
	Attributes        json.RawMessage
	MinCount          int
	MaxCount          int
	CountMultiple     int
	AllowCrossRegion  bool
	Query             string
	StringProperties  map[string]string
	NumericProperties map[string]float64
	Status            Status
	MatchID           string
	MatchAddress      string
	MatchProtocol     string
	// FailureReason is a machine-readable reason a ticket ended in
	// StatusFailed ("expired", "attempts_exhausted"). Empty for
	// non-failed tickets.
	FailureReason string
	CreatedAt     time.Time
	MatchedAt     *time.Time
	ExpiresAt     *time.Time
}

// EnqueueRequest is the input HTTP handlers pass to Queue.Enqueue. FleetID
// resolves to the fleet template the allocation should be drawn from
// (fleet_allocation mode only — zero means no fleet); the HTTP handler
// resolves a caller-supplied fleet name to an id before constructing the
// request.
type EnqueueRequest struct {
	TenantID          int64
	ProjectID         int64
	FleetID           int64
	PlayerID          int64
	Mode              Mode
	Region            string
	GameMode          string
	Attributes        json.RawMessage
	MinCount          int
	MaxCount          int
	CountMultiple     int
	AllowCrossRegion  bool
	Query             string
	StringProperties  map[string]string
	NumericProperties map[string]float64
	// ExpiresAt bounds how long the ticket may sit queued; nil disables
	// expiry.
	ExpiresAt *time.Time
}

// normalize fills zero-value mode and counts with their defaults so
// programmatic callers (tests, internal enqueues) don't have to spell out
// what the HTTP layer would have inferred. Invalid non-zero values are left
// alone for the store's constraints to reject.
func (r *EnqueueRequest) normalize() {
	if r.Mode == "" {
		r.Mode = ModeMatchOnly
		if r.FleetID != 0 {
			r.Mode = ModeFleetAllocation
		}
	}
	if r.MinCount <= 0 {
		r.MinCount = 1
	}
	if r.MaxCount <= 0 {
		r.MaxCount = r.MinCount
	}
	if r.CountMultiple <= 0 {
		r.CountMultiple = 1
	}
	if r.Query == "" {
		r.Query = "*"
	}
}

// Bucket groups tickets that can be matched together. Workers process one
// bucket at a time so cross-tenant or cross-project (or cross-fleet,
// cross-mode) mixing is impossible. FleetID is zero for non-fleet modes.
type Bucket struct {
	TenantID  int64
	ProjectID int64
	Mode      Mode
	FleetID   int64
	Region    string
	GameMode  string
}

// RosterEntry is one matched player in a match roster, including the
// criteria they matched with so peers can reason about the group.
type RosterEntry struct {
	PlayerID          int64              `json:"player_id"`
	Region            string             `json:"region,omitempty"`
	StringProperties  map[string]string  `json:"string_properties,omitempty"`
	NumericProperties map[string]float64 `json:"numeric_properties,omitempty"`
	// Attributes is the player's opaque ticket attributes, passed through to
	// matched peers so match_only P2P can exchange lobby codes or endpoints
	// with no extra infrastructure. Visible to every peer in the match.
	Attributes json.RawMessage `json:"attributes,omitempty"`
}

// Match is a committed match result. Ticket rows reference it via MatchID;
// it survives missed WebSocket deliveries so players can recover the result
// by polling their ticket. Rows are retention-bounded and GC'd.
type Match struct {
	ID        string
	TenantID  int64
	ProjectID int64
	Mode      Mode
	FleetID   int64
	Address   string
	Protocol  string
	SessionID string
	JoinCode  string
	// HostPlayerID is the player peers connect to for match_only and
	// game_session results (the group's oldest ticket). Zero for
	// fleet_allocation, where the endpoint is a dedicated server.
	HostPlayerID int64
	// AllocationID identifies the fleet resource leased to this match. Zero
	// for match-only and game-session modes.
	AllocationID fleet.AllocationID
	// ClaimedAt is set when a roster player receives the match over realtime
	// or recovers it by polling before ExpiresAt.
	ClaimedAt time.Time
	Roster    []RosterEntry
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Claim is the worker's handle on a set of tickets it has staked. The
// underlying rows stay 'queued' until the worker calls Queue.CommitClaim
// (success) or Queue.ReleaseClaim (failure). A crashed worker's claim is
// recovered by the sweeper once the lease elapses.
type Claim struct {
	// ID is a UUID identifying the claim across worker invocations. The
	// SQL CommitClaim/ReleaseClaim queries key off this id, so an expired
	// or already-released claim's commit safely affects zero rows.
	ID string
	// Tickets are the rows the claim covers. The worker uses these to
	// build the Allocate request and to fan MatchReady envelopes after
	// commit.
	Tickets []*Ticket
}

// Queue is the persistence dependency the worker owns. Tenant-scoped paths
// (Enqueue, Get, Cancel) expect the caller to have a tenant id in context;
// privileged paths (ListReadyBuckets, ClaimBucket, CommitClaim, ReleaseClaim)
// run cross-tenant from the worker goroutine.
type Queue interface {
	Enqueue(ctx context.Context, req EnqueueRequest) (*Ticket, error)
	Get(ctx context.Context, id, playerID int64) (*Ticket, error)
	Cancel(ctx context.Context, id, playerID int64) error

	ListReadyBuckets(ctx context.Context) ([]Bucket, error)
	// ClaimBucket stakes a claim on up to max unclaimed queued tickets in
	// the bucket, oldest first. Returns nil when nothing was claimable
	// (another worker won the race). Tickets stay 'queued'; only
	// claim_id/claimed_at/lease columns are set. The worker forms groups
	// from the claimed set and settles each subset via CommitTickets /
	// ReleaseTickets, then returns the rest with ReturnUnmatched.
	ClaimBucket(ctx context.Context, bucket Bucket, max int, ttl time.Duration) (*Claim, error)
	// CommitTickets flips the given still-queued claim tickets to
	// 'matched' with the given match id, address, and protocol hint. It is
	// all-or-none: it flips every requested ticket or none. A drifted claim
	// (no requested ticket still queued) returns (0, nil) — the caller
	// treats it as a harmless race and lets the orphan match GC. A partial
	// commit (some but not all still queued — a member cancelled or expired
	// between claim and commit) rolls back and returns the would-be count
	// with ErrShortCommit; the caller returns the survivors via
	// ReturnTickets and deallocates any orphan server. matchAddress and
	// matchProtocol are empty for non-fleet modes; matchProtocol may also
	// be empty when the backend can't determine it.
	CommitTickets(ctx context.Context, claim *Claim, ticketIDs []int64, matchID, matchAddress, matchProtocol string) (int64, error)
	// ReleaseTickets is the worker-driven failure path for one group:
	// bumps allocation_attempts, flips to 'failed' at the cap, and clears
	// claim cols so a future claim can re-pick the tickets.
	ReleaseTickets(ctx context.Context, claim *Claim, ticketIDs []int64, maxAttempts int) error
	// ReturnUnmatched un-claims whatever the claim still holds without
	// penalty: tickets that fit no group this pass go back to waiting.
	ReturnUnmatched(ctx context.Context, claim *Claim) error
	// ReturnTickets un-claims the given still-queued claim tickets without
	// penalty. Used after a short commit to send the survivors back to
	// waiting (scoped to one group, unlike the claim-wide ReturnUnmatched);
	// terminal tickets among the ids are already un-queued and skipped.
	ReturnTickets(ctx context.Context, claim *Claim, ticketIDs []int64) error

	// CreateMatch persists a committed match result. Tenant-scoped: the
	// worker supplies a tenant context derived from the bucket.
	CreateMatch(ctx context.Context, m *Match) error
	// GetMatch returns the match by id for the tenant on ctx, or
	// ErrNotFound.
	GetMatch(ctx context.Context, id string) (*Match, error)
	// ClaimMatch atomically marks an unexpired match claimed and returns it.
	// Polling uses this so an expired lease cannot be revived while GC runs.
	ClaimMatch(ctx context.Context, id string) (*Match, error)
}

// Sweeper is an optional capability for releasing claims left by crashed
// workers. The PGQueue implements it; the MemQueue does not (in-memory state
// vanishes with the process, so stale claims aren't a concern).
type Sweeper interface {
	SweepStaleClaims(ctx context.Context, maxAttempts int) (released int64, err error)
}

// ErrNotFound is returned by Get when the ticket id is unknown or belongs
// to another tenant (RLS hides it).
var ErrNotFound = errors.New("matchmaker: ticket not found")

// ErrAlreadyTerminal is returned by Cancel when the ticket has already
// reached a terminal status.
var ErrAlreadyTerminal = errors.New("matchmaker: ticket already finalised")

// ErrTicketActive is returned by Enqueue when the player already holds a
// queued ticket in the project. Only one active ticket per player per project
// is allowed; the caller must cancel it before opening another.
var ErrTicketActive = errors.New("matchmaker: player already has an active ticket")

// TicketActiveError wraps ErrTicketActive with the id of the ticket already
// queued so the API can point the player at the ticket to cancel. errors.Is
// against ErrTicketActive matches; errors.As recovers ActiveTicketID.
type TicketActiveError struct {
	ActiveTicketID int64
}

func (e *TicketActiveError) Error() string { return ErrTicketActive.Error() }

func (e *TicketActiveError) Unwrap() error { return ErrTicketActive }

// ErrShortCommit is returned by CommitTickets when only some of the requested
// tickets were still committable: the flip is rolled back so a match is never
// formed with a partial roster.
var ErrShortCommit = errors.New("matchmaker: commit did not cover the whole group")

// Listener is an optional capability a Queue can implement to wake the
// worker on ticket inserts instead of forcing a polling tick. The Postgres
// queue uses LISTEN/NOTIFY; the in-memory queue doesn't implement it and
// the worker silently falls back to its periodic safety tick.
type Listener interface {
	Listen(ctx context.Context, fn func(Bucket)) error
}

// FailureRecorder counts tickets flipped to 'failed', keyed by the
// machine-readable reason stamped on them (failureReasonExpired,
// failureReasonAttemptsExhausted). Every flip site lives in the queue, so the
// queue reports flips through this hook and the Prometheus layer stays out of
// this package. A nil recorder is a no-op. *observability.Metrics satisfies it.
type FailureRecorder interface {
	MatchmakerTicketFailure(reason string, n int)
}

// BucketStat is a sampled per-bucket queue metric for the observability
// gauges. Region is blank for non-fleet modes, matching the worker's buckets.
// There is deliberately no game_mode dimension: it is developer-supplied free
// text and would make the gauge series count unbounded.
type BucketStat struct {
	Mode             string
	Region           string
	Depth            int64
	OldestAgeSeconds float64
}

// StatsLister is an optional Queue capability exposing a cross-tenant sample
// of queue depth and oldest-ticket age for the observability gauges. The
// PGQueue implements it (privileged, GUC-less scan); the MemQueue implements
// it for local dev and unit tests.
type StatsLister interface {
	QueueStats(ctx context.Context) ([]BucketStat, error)
}
