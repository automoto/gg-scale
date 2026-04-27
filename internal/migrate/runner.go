// Package migrate is a thin wrapper around golang-migrate/migrate so the
// command and tests share one entry point. Migrations live in db/migrations/
// and are forward-only (expand-and-contract for column changes).
package migrate

import (
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

// Runner applies golang-migrate migrations against a Postgres database.
type Runner struct {
	m *migrate.Migrate
}

// New constructs a Runner reading SQL migrations from migrationsDir and
// applying them to databaseURL (a postgres:// connection string).
func New(databaseURL, migrationsDir string) (*Runner, error) {
	dsn, err := pgxDSN(databaseURL)
	if err != nil {
		return nil, err
	}
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		return nil, fmt.Errorf("init migrate: %w", err)
	}
	return &Runner{m: m}, nil
}

// Up applies all pending migrations. ErrNoChange is treated as success.
func (r *Runner) Up() error {
	if err := r.m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// Down reverses the most recently applied migration.
func (r *Runner) Down() error {
	if err := r.m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// Version returns the current schema version and whether the schema is dirty.
// A pristine database returns (0, false, nil).
func (r *Runner) Version() (uint, bool, error) {
	v, dirty, err := r.m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	return v, dirty, err
}

// Close releases the migration runner's database and source handles.
// Returns the joined source + database close errors, if any.
func (r *Runner) Close() error {
	if r.m == nil {
		return nil
	}
	srcErr, dbErr := r.m.Close()
	return errors.Join(srcErr, dbErr)
}

// pgxDSN ensures the DSN uses the pgx5 driver scheme that golang-migrate
// understands (`pgx5://...`). Both `postgres://` and `postgresql://` are
// accepted on input.
func pgxDSN(in string) (string, error) {
	if in == "" {
		return "", errors.New("empty database URL")
	}
	switch {
	case strings.HasPrefix(in, "postgres://"):
		return "pgx5://" + strings.TrimPrefix(in, "postgres://"), nil
	case strings.HasPrefix(in, "postgresql://"):
		return "pgx5://" + strings.TrimPrefix(in, "postgresql://"), nil
	default:
		return in, nil
	}
}
