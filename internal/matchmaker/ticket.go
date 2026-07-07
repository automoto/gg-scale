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
	CreatedAt         time.Time
	MatchedAt         *time.Time
	ExpiresAt         *time.Time
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
	// MaxActive caps the player's concurrently queued tickets in the
	// project. 0 disables the cap. Enqueue returns ErrTicketLimit at the
	// cap.
	MaxActive int
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
	// 'matched' with the given match id, address, and protocol hint.
	// Returns rows-affected so the caller can detect a race (sweeper,
	// cancel) and deallocate the orphan server. matchAddress and
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

	// CreateMatch persists a committed match result. Tenant-scoped: the
	// worker supplies a tenant context derived from the bucket.
	CreateMatch(ctx context.Context, m *Match) error
	// GetMatch returns the match by id for the tenant on ctx, or
	// ErrNotFound.
	GetMatch(ctx context.Context, id string) (*Match, error)
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

// ErrTicketLimit is returned by Enqueue when the player already has
// EnqueueRequest.MaxActive queued tickets in the project.
var ErrTicketLimit = errors.New("matchmaker: too many queued tickets")

// Listener is an optional capability a Queue can implement to wake the
// worker on ticket inserts instead of forcing a polling tick. The Postgres
// queue uses LISTEN/NOTIFY; the in-memory queue doesn't implement it and
// the worker silently falls back to its periodic safety tick.
type Listener interface {
	Listen(ctx context.Context, fn func(Bucket)) error
}
