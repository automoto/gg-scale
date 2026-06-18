// Package matchmaker turns end-user "find me a match" requests into game
// server allocations. End-users POST tickets through HTTP; a background
// worker batches them by bucket (tenant, project, fleet, region, game_mode),
// stakes a claim on the bucket, calls fleet.Manager.Allocate when a bucket
// fills, then commits the claim — flipping the rows to 'matched' and pushing
// a MatchReady envelope back to each player over the realtime hub.
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

// Ticket is one in-flight matchmaking request.
type Ticket struct {
	ID            int64
	TenantID      int64
	ProjectID     int64
	FleetID       int64
	EndUserID     int64
	Region        string
	GameMode      string
	Attributes    json.RawMessage
	Status        Status
	MatchAddress  string
	MatchProtocol string
	CreatedAt     time.Time
	MatchedAt     *time.Time
}

// EnqueueRequest is the input HTTP handlers pass to Queue.Enqueue. FleetID
// resolves to the fleet template the allocation should be drawn from; the
// HTTP handler resolves a caller-supplied fleet name to an id before
// constructing the request.
type EnqueueRequest struct {
	TenantID   int64
	ProjectID  int64
	FleetID    int64
	EndUserID  int64
	Region     string
	GameMode   string
	Attributes json.RawMessage
}

// Bucket groups tickets that can be matched together. Workers process one
// bucket at a time so cross-tenant or cross-project (or cross-fleet) mixing
// is impossible.
type Bucket struct {
	TenantID  int64
	ProjectID int64
	FleetID   int64
	Region    string
	GameMode  string
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
	Get(ctx context.Context, id, endUserID int64) (*Ticket, error)
	Cancel(ctx context.Context, id, endUserID int64) error

	ListReadyBuckets(ctx context.Context, minTickets int) ([]Bucket, error)
	// ClaimBucket stakes a claim on up to n unclaimed queued tickets in the
	// bucket. Returns nil on short count (another worker won the race);
	// the caller's contract is "skip this bucket, the next tick or notify
	// will retry." Tickets stay 'queued'; only claim_id/claimed_at/lease
	// columns are set.
	ClaimBucket(ctx context.Context, bucket Bucket, n int, ttl time.Duration) (*Claim, error)
	// CommitClaim flips every still-queued row holding this claim to
	// 'matched' with the given address and protocol hint. Returns
	// rows-affected so the caller can detect a race (sweeper, cancel)
	// and deallocate the orphan server. matchProtocol may be empty when
	// the backend can't determine it.
	CommitClaim(ctx context.Context, claim *Claim, matchAddress, matchProtocol string) (int64, error)
	// ReleaseClaim is the worker-driven failure path: bumps
	// allocation_attempts, flips to 'failed' at the cap, and clears
	// claim cols so a future claim can re-pick the ticket.
	ReleaseClaim(ctx context.Context, claim *Claim, maxAttempts int) error
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

// Listener is an optional capability a Queue can implement to wake the
// worker on ticket inserts instead of forcing a polling tick. The Postgres
// queue uses LISTEN/NOTIFY; the in-memory queue doesn't implement it and
// the worker silently falls back to its periodic safety tick.
type Listener interface {
	Listen(ctx context.Context, fn func(Bucket)) error
}
