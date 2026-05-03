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
	pool *pgxpool.Pool
}

// NewPool constructs a Pool from an existing pgxpool. The caller retains
// ownership of the pgxpool's lifecycle.
func NewPool(p *pgxpool.Pool) *Pool {
	return &Pool{pool: p}
}

// Q runs fn inside a transaction with `app.tenant_id` set to the tenant
// extracted from ctx. The transaction is committed if fn returns nil and
// rolled back otherwise.
func (p *Pool) Q(ctx context.Context, fn func(pgx.Tx) error) error {
	tenantID, err := TenantFromContext(ctx)
	if err != nil {
		return err
	}

	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	if _, err = tx.Exec(ctx,
		"SELECT set_config('app.tenant_id', $1, true)",
		strconv.FormatInt(tenantID, 10),
	); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("set app.tenant_id: %w", err)
	}

	if err = fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// BootstrapQ runs fn inside a transaction before a tenant context exists.
// Keep this for narrow bootstrap paths such as dashboard tenant creation;
// tenant-scoped request handlers should use Q so RLS receives app.tenant_id.
func (p *Pool) BootstrapQ(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	if err = fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
