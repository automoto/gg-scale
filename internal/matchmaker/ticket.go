// Package matchmaker turns end-user "find me a match" requests into game
// server allocations. End-users POST tickets through HTTP; a background
// worker batches them by bucket (tenant, project, region, game_mode), calls
// fleet.Manager.Allocate when a bucket fills, and pushes a MatchReady
// envelope back to each player over the realtime hub.
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
	ID           int64
	TenantID     int64
	ProjectID    int64
	EndUserID    int64
	Region       string
	GameMode     string
	Attributes   json.RawMessage
	Status       Status
	MatchAddress string
	CreatedAt    time.Time
	MatchedAt    *time.Time
}

// EnqueueRequest is the input HTTP handlers pass to Queue.Enqueue.
type EnqueueRequest struct {
	TenantID   int64
	ProjectID  int64
	EndUserID  int64
	Region     string
	GameMode   string
	Attributes json.RawMessage
}

// Bucket groups tickets that can be matched together. Workers process one
// bucket at a time so cross-tenant or cross-project mixing is impossible.
type Bucket struct {
	TenantID  int64
	ProjectID int64
	Region    string
	GameMode  string
}

// Queue is the persistence dependency the worker owns. Tenant-scoped paths
// (Enqueue, Get, Cancel) expect the caller to have a tenant id in context;
// privileged paths (ListReadyBuckets, PopBucket, MarkMatched, MarkFailed)
// run cross-tenant from the worker goroutine.
type Queue interface {
	Enqueue(ctx context.Context, req EnqueueRequest) (*Ticket, error)
	Get(ctx context.Context, id int64) (*Ticket, error)
	Cancel(ctx context.Context, id int64) error

	ListReadyBuckets(ctx context.Context, minTickets int) ([]Bucket, error)
	PopBucket(ctx context.Context, bucket Bucket, n int) ([]*Ticket, error)
	MarkMatched(ctx context.Context, ids []int64, address string) error
	MarkFailed(ctx context.Context, ids []int64) error
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
