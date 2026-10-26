package relaynode

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

// uniqueViolation is the Postgres SQLSTATE for a unique index conflict.
const uniqueViolation = "23505"

// Store reads and writes the relay_nodes table. The table is platform-global,
// so every query runs outside a tenant context.
type Store struct {
	pool *db.Pool
	now  func() time.Time
}

// NewStore returns a Store on pool.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool, now: time.Now}
}

// Create validates spec and registers a known node. It returns a wrapped
// ErrInvalid or ErrDuplicate for a bad request.
func (s *Store) Create(ctx context.Context, spec Spec) (Node, error) {
	if err := spec.Validate(); err != nil {
		return Node{}, err
	}
	var row sqlcgen.RelayNode
	err := s.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).CreateRelayNode(ctx, sqlcgen.CreateRelayNodeParams{
			Region:       spec.Region,
			TurnUrls:     spec.TURNURLs,
			LifecycleUrl: spec.LifecycleURL,
		})
		return qerr
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return Node{}, ErrDuplicate
	}
	if err != nil {
		return Node{}, err
	}
	return s.toNode(row), nil
}

// List returns every node, retired ones included, oldest first.
func (s *Store) List(ctx context.Context) ([]Node, error) {
	return s.list(ctx, func(q *sqlcgen.Queries) ([]sqlcgen.RelayNode, error) { return q.ListRelayNodes(ctx) })
}

// Known returns the nodes the probe job reads.
func (s *Store) Known(ctx context.Context) ([]Node, error) {
	return s.list(ctx, func(q *sqlcgen.Queries) ([]sqlcgen.RelayNode, error) { return q.ListKnownRelayNodes(ctx) })
}

// Eligible returns the nodes relay credentials may advertise.
func (s *Store) Eligible(ctx context.Context) ([]Node, error) {
	return s.list(ctx, func(q *sqlcgen.Queries) ([]sqlcgen.RelayNode, error) {
		return q.ListEligibleRelayNodes(ctx, FreshnessWindow.Seconds())
	})
}

func (s *Store) list(ctx context.Context, query func(*sqlcgen.Queries) ([]sqlcgen.RelayNode, error)) ([]Node, error) {
	var rows []sqlcgen.RelayNode
	err := s.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		rows, qerr = query(sqlcgen.New(tx))
		return qerr
	})
	if err != nil {
		return nil, err
	}
	nodes := make([]Node, 0, len(rows))
	for _, row := range rows {
		nodes = append(nodes, s.toNode(row))
	}
	return nodes, nil
}

// Retire stops advertising and probing a node. It keeps the row and is
// idempotent. It returns ErrNotFound for an unknown id.
func (s *Store) Retire(ctx context.Context, id int64) (Node, error) {
	var row sqlcgen.RelayNode
	err := s.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).RetireRelayNode(ctx, id)
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, err
	}
	return s.toNode(row), nil
}

// RecordProbe stores one probe result. A node retired since it was listed is
// left alone.
func (s *Store) RecordProbe(ctx context.Context, id int64, r Result) error {
	params := sqlcgen.RecordRelayNodeProbeParams{ID: id, Ok: r.OK, ActiveAllocations: r.ActiveWork}
	if r.State != "" {
		state := r.State
		params.LifecycleState = &state
	}
	return s.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).RecordRelayNodeProbe(ctx, params)
	})
}

func (s *Store) toNode(row sqlcgen.RelayNode) Node {
	n := Node{
		ID:                    row.ID,
		Region:                row.Region,
		TURNURLs:              row.TurnUrls,
		LifecycleURL:          row.LifecycleUrl,
		State:                 row.State,
		LastProbeAt:           timePtr(row.LastProbeAt),
		LastProbeOK:           row.LastProbeOk,
		LastLifecycleState:    row.LastLifecycleState,
		LastActiveAllocations: row.LastActiveAllocations,
		CreatedAt:             row.CreatedAt.Time,
		UpdatedAt:             row.UpdatedAt.Time,
	}
	n.Eligible = n.eligibleAt(s.now())
	return n
}

func timePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}
