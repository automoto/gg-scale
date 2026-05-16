// Package db wraps pgxpool with a tenant-aware transaction helper. Q(ctx)
// is the load-bearing entry point: it reads tenant_id from context, opens a
// transaction, sets the `app.tenant_id` GUC via SET LOCAL so RLS policies
// in 0009_rls.up.sql can filter, and runs the caller's closure inside it.
//
// The tenant middleware (internal/tenant) is the only intended writer of
// the context value; sqlc-generated query code calls Q(ctx) and never
// touches the GUC directly.
package db

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoTenant is returned when a code path that requires tenant scoping is
// invoked with a context that carries no tenant_id. RLS would reject the
// query downstream anyway; failing here gives a clearer error.
var ErrNoTenant = errors.New("db: no tenant in context")

type tenantKey struct{}
type projectKey struct{}

// WithTenant returns a derived context that carries tenantID. A zero value
// is treated as absent; TenantFromContext will return ErrNoTenant.
func WithTenant(ctx context.Context, tenantID int64) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenantID)
}

// TenantFromContext extracts the tenant_id installed by WithTenant. Returns
// ErrNoTenant when the context has no tenant or carries a zero id.
func TenantFromContext(ctx context.Context) (int64, error) {
	v, ok := ctx.Value(tenantKey{}).(int64)
	if !ok || v == 0 {
		return 0, ErrNoTenant
	}
	return v, nil
}

// WithProject returns a derived context carrying projectID. Project context
// is optional — many tenant-scoped routes don't require it.
func WithProject(ctx context.Context, projectID int64) context.Context {
	return context.WithValue(ctx, projectKey{}, projectID)
}

// ProjectFromContext returns the project_id installed by WithProject and a
// boolean indicating whether one was set.
func ProjectFromContext(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(projectKey{}).(int64)
	if !ok || v == 0 {
		return 0, false
	}
	return v, true
}

// Pool wraps a pgxpool with a tenant-aware transaction helper.
type Pool struct {
	pool             *pgxpool.Pool
	statementTimeout time.Duration
}

// NewPool constructs a Pool from an existing pgxpool. The caller retains
// ownership of the pgxpool's lifecycle.
func NewPool(p *pgxpool.Pool) *Pool {
	return &Pool{pool: p}
}

// NewPoolWithTimeout is like NewPool but also installs a per-transaction
// statement timeout. Zero means "leave the server default."
func NewPoolWithTimeout(p *pgxpool.Pool, statementTimeout time.Duration) *Pool {
	return &Pool{pool: p, statementTimeout: statementTimeout}
}

// Q runs fn inside a transaction with `app.tenant_id` set to the tenant
// extracted from ctx. The transaction is committed if fn returns nil and
// rolled back otherwise. A deferred rollback guarded by `committed` runs on
// every exit path — including a commit error — so a partially failed
// commit cannot leak a dangling tx into the connection pool.
func (p *Pool) Q(ctx context.Context, fn func(pgx.Tx) error) error {
	tenantID, err := TenantFromContext(ctx)
	if err != nil {
		return err
	}

	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	if _, err = tx.Exec(ctx,
		"SELECT set_config('app.tenant_id', $1, true)",
		strconv.FormatInt(tenantID, 10),
	); err != nil {
		return fmt.Errorf("set app.tenant_id: %w", err)
	}

	if p.statementTimeout > 0 {
		ms := p.statementTimeout.Milliseconds()
		if _, err = tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", ms)); err != nil {
			return fmt.Errorf("set statement_timeout: %w", err)
		}
	}

	if err = fn(tx); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// ListenChannel acquires a dedicated connection, issues LISTEN <channel>,
// and dispatches each notification's payload to fn. Returns nil on
// ctx.Done() and a wrapped error on connection failure (the caller decides
// whether to reconnect — typically a background loop with backoff).
//
// The underlying connection is force-closed on return so pgxpool evicts
// it rather than returning a LISTEN-tainted conn to the shared pool. pgx
// does not auto-issue UNLISTEN on Release, and a later Q()/BootstrapQ()
// borrowing that conn would receive async NOTIFY frames mid-query.
func (p *Pool) ListenChannel(ctx context.Context, channel string, fn func(payload string)) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("listen acquire: %w", err)
	}
	defer func() {
		// Force the pool to discard this conn rather than reuse it.
		_ = conn.Conn().Close(context.Background())
		conn.Release()
	}()

	// Postgres LISTEN does not accept parameterized identifiers; quote via
	// pgx.Identifier so a future dynamic channel name can't be injected.
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("wait notification: %w", err)
		}
		fn(n.Payload)
	}
}

// BootstrapQ runs fn inside a transaction before a tenant context exists.
// Keep this for narrow bootstrap paths such as dashboard tenant creation;
// tenant-scoped request handlers should use Q so RLS receives app.tenant_id.
func (p *Pool) BootstrapQ(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}
