package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ggscale/ggscale/internal/db"
)

func TestWithTenant_round_trips_tenant_id(t *testing.T) {
	ctx := db.WithTenant(context.Background(), 42)

	id, err := db.TenantFromContext(ctx)

	assert.NoError(t, err)
	assert.Equal(t, int64(42), id)
}

func TestTenantFromContext_returns_ErrNoTenant_on_bare_context(t *testing.T) {
	_, err := db.TenantFromContext(context.Background())

	assert.True(t, errors.Is(err, db.ErrNoTenant))
}

func TestWithTenant_zero_id_returns_ErrNoTenant(t *testing.T) {
	ctx := db.WithTenant(context.Background(), 0)

	_, err := db.TenantFromContext(ctx)

	assert.True(t, errors.Is(err, db.ErrNoTenant))
}
