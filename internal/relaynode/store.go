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
}

// NewStore returns a Store on pool.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool}
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
	return toNode(row), nil
}

// List returns every node, retired ones included, oldest first. Eligible is
// computed by the database, with the same rule and clock as Eligible.
func (s *Store) List(ctx context.Context) ([]Node, error) {
	var rows []sqlcgen.ListRelayNodesRow
	err := s.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		rows, qerr = sqlcgen.New(tx).ListRelayNodes(ctx, FreshnessWindow.Seconds())
		return qerr
	})
	if err != nil {
		return nil, err
	}
	nodes := make([]Node, 0, len(rows))
	for _, row := range rows {
		n := toNode(row.RelayNode)
		n.Eligible = row.Eligible
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// Known returns the nodes the probe job reads.
func (s *Store) Known(ctx context.Context) ([]Node, error) {
	return s.list(ctx, func(q *sqlcgen.Queries) ([]sqlcgen.RelayNode, error) { return q.ListKnownRelayNodes(ctx) })
}

// Eligible returns the nodes relay credentials may advertise.
func (s *Store) Eligible(ctx context.Context) ([]Node, error) {
	nodes, err := s.list(ctx, func(q *sqlcgen.Queries) ([]sqlcgen.RelayNode, error) {
		return q.ListEligibleRelayNodes(ctx, FreshnessWindow.Seconds())
	})
	for i := range nodes {
		nodes[i].Eligible = true
	}
	return nodes, err
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
		nodes = append(nodes, toNode(row))
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
	return toNode(row), nil
}

// PriorProbe is a node's stored probe result just before a new one replaced
// it.
type PriorProbe struct {
	At    *time.Time
	OK    bool
	Error *string
}

// RecordProbe stores one probe result and returns the result it replaced,
// read under a row lock so concurrent probers each see the other's write.
// recorded is false when the node was retired since it was listed; such a
// row is left alone.
func (s *Store) RecordProbe(ctx context.Context, id int64, r Result) (prior PriorProbe, recorded bool, err error) {
	params := sqlcgen.RecordRelayNodeProbeParams{NodeID: id, Ok: r.OK, ActiveAllocations: r.ActiveWork}
	if !r.OK {
		failure := r.Failure
		params.ProbeError = &failure
	}
	if r.State != "" {
		state := r.State
		params.LifecycleState = &state
	}
	var row sqlcgen.RecordRelayNodeProbeRow
	err = s.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).RecordRelayNodeProbe(ctx, params)
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return PriorProbe{}, false, nil
	}
	if err != nil {
		return PriorProbe{}, false, err
	}
	return PriorProbe{At: timePtr(row.PrevProbeAt), OK: row.PrevProbeOk, Error: row.PrevProbeError}, true, nil
}

// toNode converts a row. Eligible is left false: only a query that computes
// eligibility sets it.
func toNode(row sqlcgen.RelayNode) Node {
	return Node{
		ID:                    row.ID,
		Region:                row.Region,
		TURNURLs:              row.TurnUrls,
		LifecycleURL:          row.LifecycleUrl,
		State:                 row.State,
		LastProbeAt:           timePtr(row.LastProbeAt),
		LastProbeOK:           row.LastProbeOk,
		LastProbeError:        row.LastProbeError,
		LastLifecycleState:    row.LastLifecycleState,
		LastActiveAllocations: row.LastActiveAllocations,
		CreatedAt:             row.CreatedAt.Time,
		UpdatedAt:             row.UpdatedAt.Time,
	}
}

func timePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}
