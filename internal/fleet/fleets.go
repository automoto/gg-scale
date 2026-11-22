package fleet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/secretseal"
)

// Fleet is an operator-defined template that allocations are drawn from. It
// captures the backend-specific recipe (Agones fleet name+selector labels or
// opaque plugin config) under a project-scoped name. Allocations reference a
// Fleet by id; the matchmaker API and SDK identify a Fleet by its
// project-scoped Name.
//
// Config is the per-backend payload, flattened to a string map. For agones
// the keys are "namespace" (optional override), "fleet_name", and any
// selector labels passed under "selector.<key>" entries. Plugin templates
// are free-form and pass through verbatim to the plugin RPC.
type Fleet struct {
	ID        int64
	TenantID  int64
	ProjectID int64
	Name      string
	Backend   string
	Config    map[string]string
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time

	// configErr is ErrConfigNotOpened when a sealed plugin config value did
	// not open; Config then has an empty value for that key.
	configErr error
}

// OpenedConfig returns the config for a use that needs the values: an
// allocation, a plugin cleanup, or a write that keeps a stored value. It
// returns ErrConfigNotOpened, and logs the fleet id, when a sealed value did
// not open.
func (f *Fleet) OpenedConfig(ctx context.Context) (map[string]string, error) {
	if f.configErr != nil {
		slog.ErrorContext(ctx, "fleet: a sealed plugin config value does not open; check CREDENTIAL_ENC_KEY", "fleet_id", f.ID)
		return nil, f.configErr
	}
	return f.Config, nil
}

// FleetCreate is the input shape for CreateFleet. Tenant id is read
// from the request context via RLS; passing it as a parameter would let
// callers bypass the policy. The Fleet prefix disambiguates it from
// fleet.Store, which serves allocations rather than templates.
//
//nolint:revive // intentional Fleet-prefix to disambiguate from fleet.Store.
type FleetCreate struct {
	ProjectID int64
	Name      string
	Backend   string
	Config    map[string]string
}

// FleetStore persists fleet templates. Implementations live below
// (Postgres-backed) and in tests (in-memory). Distinct from fleet.Store,
// which serves allocations.
//
//nolint:revive // intentional Fleet-prefix to disambiguate from fleet.Store.
type FleetStore interface {
	GetByID(ctx context.Context, id int64) (*Fleet, error)
	GetByName(ctx context.Context, projectID int64, name string) (*Fleet, error)
	ListForProject(ctx context.Context, projectID int64) ([]*Fleet, error)
	ListDeletedForProject(ctx context.Context, projectID int64) ([]*Fleet, error)
}

// ErrFleetNotFound is returned when a fleet id or name does not resolve to
// an active row.
var ErrFleetNotFound = errors.New("fleet: fleet template not found")

// PostgresFleetStore is the production FleetStore. It opens sealed plugin
// config values with cipher, so callers get plain values.
type PostgresFleetStore struct {
	pool   *db.Pool
	cipher *secretseal.Cipher
}

// NewPostgresFleetStore returns a FleetStore bound to the given pool. cipher
// is the credential cipher that CreateFleet and UpdateFleet seal with; nil
// reads values as they are stored.
func NewPostgresFleetStore(pool *db.Pool, cipher *secretseal.Cipher) *PostgresFleetStore {
	return &PostgresFleetStore{pool: pool, cipher: cipher}
}

// GetByID looks up a fleet by primary key. Returns ErrFleetNotFound when the
// id is unknown to the current tenant.
func (s *PostgresFleetStore) GetByID(ctx context.Context, id int64) (*Fleet, error) {
	var out *Fleet
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).GetFleetByID(ctx, id)
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrFleetNotFound
			}
			return qerr
		}
		f, ferr := fleetRowToModel(s.cipher, row)
		if ferr != nil {
			return ferr
		}
		out = f
		return nil
	})
	return out, err
}

// GetByName resolves a project-scoped name to a fleet. Soft-deleted rows
// are excluded.
func (s *PostgresFleetStore) GetByName(ctx context.Context, projectID int64, name string) (*Fleet, error) {
	var out *Fleet
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).GetFleetByName(ctx, sqlcgen.GetFleetByNameParams{
			ProjectID: projectID,
			Name:      name,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrFleetNotFound
			}
			return qerr
		}
		f, ferr := fleetRowToModel(s.cipher, row)
		if ferr != nil {
			return ferr
		}
		out = f
		return nil
	})
	return out, err
}

// ListForProject returns active fleet templates for a project, ordered by
// name.
func (s *PostgresFleetStore) ListForProject(ctx context.Context, projectID int64) ([]*Fleet, error) {
	var out []*Fleet
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlcgen.New(tx).ListFleetsForProject(ctx, projectID)
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			f, ferr := fleetRowToModel(s.cipher, row)
			if ferr != nil {
				return ferr
			}
			out = append(out, f)
		}
		return nil
	})
	return out, err
}

// ListDeletedForProject returns the deleted fleet templates of a project,
// newest deletion first.
func (s *PostgresFleetStore) ListDeletedForProject(ctx context.Context, projectID int64) ([]*Fleet, error) {
	var out []*Fleet
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlcgen.New(tx).ListDeletedFleetsForProject(ctx, projectID)
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			f, ferr := fleetRowToModel(s.cipher, row)
			if ferr != nil {
				return ferr
			}
			out = append(out, f)
		}
		return nil
	})
	return out, err
}

func fleetRowToModel(cipher *secretseal.Cipher, row sqlcgen.Fleet) (*Fleet, error) {
	cfg, err := decodeFleetConfig(row.Config)
	if err != nil {
		return nil, err
	}
	f := &Fleet{
		ID:        row.ID,
		TenantID:  row.TenantID,
		ProjectID: row.ProjectID,
		Name:      row.Name,
		Backend:   row.Backend,
		Config:    cfg,
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
	if row.DeletedAt.Valid {
		t := row.DeletedAt.Time
		f.DeletedAt = &t
	}
	if ConfigIsSecret(row.Backend) {
		f.configErr = openConfig(cipher, cfg)
	}
	return f, nil
}

// sealedValuePrefix marks a sealed config value. The secretseal frame is
// binary, so a stored value is this prefix and the base64 of the frame.
const sealedValuePrefix = "ggseal1:"

// ConfigIsSecret reports whether the config values of a backend are sealed
// at rest and kept out of the dashboard. Only agones config is known and has
// no secret; a plugin config is free-form.
func ConfigIsSecret(backend string) bool {
	return backend != "agones"
}

// encodeFleetConfig returns the stored JSON of a config. With a cipher, each
// value of a secret config is sealed; keys stay plain.
func encodeFleetConfig(cipher *secretseal.Cipher, backend string, m map[string]string) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	if cipher != nil && ConfigIsSecret(backend) {
		sealed := make(map[string]string, len(m))
		for k, v := range m {
			frame, err := cipher.Encrypt([]byte(v))
			if err != nil {
				return nil, fmt.Errorf("fleet: seal config: %w", err)
			}
			sealed[k] = sealedValuePrefix + base64.StdEncoding.EncodeToString(frame)
		}
		m = sealed
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("fleet: encode config: %w", err)
	}
	return b, nil
}

// openConfig opens the sealed values of a plugin config in place. A value
// without the prefix is plain (a row written before sealing, or with no
// cipher). A sealed value that does not open is set to empty, so its sealed
// text never reaches a caller, and ErrConfigNotOpened is returned. Only a
// plugin config is opened: an agones config is returned by the API, so
// opening it would decrypt any frame that a caller puts in it.
func openConfig(cipher *secretseal.Cipher, cfg map[string]string) error {
	var err error
	for k, v := range cfg {
		encoded, ok := strings.CutPrefix(v, sealedValuePrefix)
		if !ok {
			continue
		}
		plain, ok := openSealed(cipher, encoded)
		if !ok {
			err = ErrConfigNotOpened
		}
		cfg[k] = plain
	}
	return err
}

func openSealed(cipher *secretseal.Cipher, encoded string) (string, bool) {
	if cipher == nil {
		return "", false
	}
	frame, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	plain, err := cipher.Decrypt(frame)
	if err != nil {
		return "", false
	}
	return string(plain), true
}

func decodeFleetConfig(b []byte) (map[string]string, error) {
	if len(b) == 0 {
		return map[string]string{}, nil
	}
	var out map[string]string
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("fleet: decode config: %w", err)
	}
	if out == nil {
		out = map[string]string{}
	}
	return out, nil
}
