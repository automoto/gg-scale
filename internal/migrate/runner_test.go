package migrate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/stub"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStubRunner builds a Runner over two bundled migrations (versions 1 and 2)
// and a stub database already at dbVersion.
func newStubRunner(t *testing.T, dbVersion int, dirty bool) (*Runner, *stub.Stub) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"0001_a.up.sql", "0001_a.down.sql", "0002_b.up.sql", "0002_b.down.sql"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("SELECT 1;"), 0o600))
	}
	src, err := source.Open("file://" + dir)
	require.NoError(t, err)
	drv, err := stub.WithInstance(nil, &stub.Config{})
	require.NoError(t, err)
	db := drv.(*stub.Stub)
	require.NoError(t, db.SetVersion(dbVersion, dirty))
	m, err := migrate.NewWithInstance("file", src, "stub", db)
	require.NoError(t, err)
	r := &Runner{m: m, src: src}
	t.Cleanup(func() { _ = r.Close() })
	return r, db
}

func TestUp_should_start_when_db_is_ahead_of_bundled_migrations(t *testing.T) {
	r, db := newStubRunner(t, 5, false)

	err := r.Up()

	assert.NoError(t, err)
	assert.Empty(t, db.MigrationSequence)
}

func TestUp_should_fail_when_db_is_ahead_and_dirty(t *testing.T) {
	r, _ := newStubRunner(t, 5, true)

	err := r.Up()

	var dirtyErr migrate.ErrDirty
	assert.True(t, errors.As(err, &dirtyErr), "got %v", err)
}

func TestUp_should_apply_pending_migrations_when_db_is_behind(t *testing.T) {
	r, db := newStubRunner(t, 1, false)

	require.NoError(t, r.Up())

	assert.Equal(t, 2, db.CurrentVersion)
}
