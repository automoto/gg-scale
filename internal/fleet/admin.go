package fleet

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/projectadmin"
	"github.com/automoto/gg-scale/internal/webutil"
)

// Limits on a fleet config, for the dashboard form and the management API.
const (
	MaxConfigKeys     = 64
	MaxConfigKeyOrVal = 256
)

// Audit actions of the fleet writes. The dashboard and tokens use the same
// names, with different actors.
const (
	actionCreate  = "fleet.template.create"
	actionUpdate  = "fleet.template.update"
	actionDelete  = "fleet.template.delete"
	actionRestore = "fleet.template.restore"

	actionAllocate   = "fleet.allocate.manual"
	actionDeallocate = "fleet.deallocate.manual"
)

var (
	// errNoFleetStore means the manager has no Postgres fleet store, so the
	// fleet writes cannot run.
	errNoFleetStore = errors.New("fleet: no fleet store for writes")
	// ErrFleetNameTaken means a live fleet of the project has the name.
	ErrFleetNameTaken = errors.New("fleet: a fleet with that name already exists in this Game Project")
)

// InvalidError means the name or config is not valid. The text names the
// field and never the value, because a plugin config can hold a secret. The
// dashboard and the management API show it as it is.
type InvalidError string

func (e InvalidError) Error() string { return string(e) }

// ErrConfigNotOpened means a sealed value of a plugin config does not open
// with the credential key (the key was lost or changed). The value is not
// used: a write that keeps it, an allocation, and a plugin cleanup are
// refused until the config is entered again.
const ErrConfigNotOpened = InvalidError("A saved plugin config value cannot be opened. Enter the plugin config again.")

// ValidateConfig checks a fleet config. Its messages never carry a value.
// An agones config needs fleet_name and may have namespace and selector.*
// keys: at most MaxConfigKeys selector keys, not counting the two fixed keys.
// A plugin config is free-form, with at most MaxConfigKeys keys. A key
// (without the selector prefix) or value is at most MaxConfigKeyOrVal bytes.
func ValidateConfig(backend string, cfg map[string]string) error {
	agones := backend == "agones"
	keys := 0
	for k, v := range cfg {
		name, fixed := k, false
		if agones {
			switch {
			case k == "fleet_name" || k == "namespace":
				fixed = true
			case strings.HasPrefix(k, selectorPrefix):
				name = strings.TrimPrefix(k, selectorPrefix)
				keys++
			default:
				return InvalidError("Unknown agones config key.")
			}
		} else {
			keys++
		}
		if !webutil.ValidText(k) || !webutil.ValidText(v) {
			return InvalidError("A config key or value is not valid text.")
		}
		if len(v) > MaxConfigKeyOrVal || len(name) > MaxConfigKeyOrVal || (!fixed && name == "") {
			return InvalidError("A config key or value is empty or too long.")
		}
	}
	if keys > MaxConfigKeys {
		return InvalidError("Too many config keys.")
	}
	if agones && strings.TrimSpace(cfg["fleet_name"]) == "" {
		return InvalidError("Fleet name is required.")
	}
	return nil
}

const selectorPrefix = "selector."

func validateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return InvalidError("Name is required.")
	}
	if !ValidText(name) {
		return InvalidError("Name is not valid or too long.")
	}
	return nil
}

// PluginNamePattern is the set of plugin names: lowercase letters, digits,
// "_" and "-", at most 64 characters, starting with a letter or digit. It
// matches the plugin binary name ggscale-fleet-<name> in FLEET_PLUGIN_DIR.
var PluginNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// validateBackend accepts agones, the bare name plugin (kept for older
// fleets) and plugin:<name> with a name that PluginNamePattern allows.
func validateBackend(backend string) error {
	if backend == "agones" || backend == "plugin" {
		return nil
	}
	if name, ok := strings.CutPrefix(backend, "plugin:"); ok && PluginNamePattern.MatchString(name) {
		return nil
	}
	return InvalidError("Backend must be agones or plugin:<name> (name: lowercase letters, digits, _ and -, at most 64 characters).")
}

// BackendMatches reports whether a fleet stored with backend stored can run on
// a manager whose backend is named running. A stored bare "plugin" matches any
// "plugin:<name>" manager.
func BackendMatches(stored, running string) bool {
	return stored == running || (stored == "plugin" && strings.HasPrefix(running, "plugin:"))
}

func translateName(err error) error {
	if webutil.IsUniqueViolation(err) {
		return ErrFleetNameTaken
	}
	return err
}

// configKeys returns the sorted keys. It is never nil, so the audit row has
// [] and not null for no config.
func configKeys(cfg map[string]string) []string {
	keys := slices.AppendSeq(make([]string, 0, len(cfg)), maps.Keys(cfg))
	slices.Sort(keys)
	return keys
}

// CreateFleet inserts a fleet and its audit row in one transaction. It does
// no authorization. The values of a plugin config are sealed with the
// cipher of the store; with a nil cipher they are stored plain. The audit
// payload has config keys, never values.
func (s *PostgresFleetStore) CreateFleet(ctx context.Context, tenantID int64, in FleetCreate, actor projectadmin.Actor) (*Fleet, error) {
	if s == nil {
		return nil, errNoFleetStore
	}
	if err := validateName(in.Name); err != nil {
		return nil, err
	}
	if err := validateBackend(in.Backend); err != nil {
		return nil, err
	}
	if err := ValidateConfig(in.Backend, in.Config); err != nil {
		return nil, err
	}
	cfg, err := encodeFleetConfig(s.cipher, in.Backend, in.Config)
	if err != nil {
		return nil, err
	}
	var out *Fleet
	err = s.pool.Q(db.WithTenant(ctx, tenantID), func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).CreateFleet(ctx, sqlcgen.CreateFleetParams{
			ProjectID: in.ProjectID, Name: in.Name, Backend: in.Backend, Config: cfg,
		})
		if qerr != nil {
			return translateName(qerr)
		}
		f, ferr := fleetRowToModel(s.cipher, row)
		if ferr != nil {
			return ferr
		}
		out = f
		return actor.Audit(ctx, tx, tenantID, in.ProjectID, actionCreate, strconv.FormatInt(f.ID, 10), map[string]any{
			"fleet_name": f.Name, "backend": f.Backend, "config_keys": configKeys(in.Config),
		})
	})
	if err != nil {
		return nil, fmt.Errorf("fleet: create fleet: %w", err)
	}
	return out, nil
}

// UpdateFleet changes the name and config of a live fleet of the project. A
// nil name or config keeps the stored value. The config is written again on
// every update, so the values of a plugin config are sealed. The
// backend is fixed. It returns ErrFleetNotFound for a deleted fleet or a
// fleet of another project.
func (s *PostgresFleetStore) UpdateFleet(ctx context.Context, tenantID, projectID, id int64, name *string, config map[string]string, actor projectadmin.Actor) (*Fleet, error) {
	if s == nil {
		return nil, errNoFleetStore
	}
	if name != nil {
		if err := validateName(*name); err != nil {
			return nil, err
		}
	}
	var out *Fleet
	err := s.pool.Q(db.WithTenant(ctx, tenantID), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		cur, qerr := q.GetProjectFleet(ctx, sqlcgen.GetProjectFleetParams{ProjectID: projectID, ID: id})
		if errors.Is(qerr, pgx.ErrNoRows) || (qerr == nil && cur.DeletedAt.Valid) {
			return ErrFleetNotFound
		}
		if qerr != nil {
			return qerr
		}
		newName := cur.Name
		if name != nil {
			newName = *name
		}
		if config != nil {
			if err := ValidateConfig(cur.Backend, config); err != nil {
				return err
			}
		}
		if config == nil {
			// Write the stored config again, so a row stored before
			// sealing is sealed now. A value that does not open is never
			// sealed again.
			stored, err := fleetRowToModel(s.cipher, cur)
			if err != nil {
				return err
			}
			if config, err = stored.OpenedConfig(ctx); err != nil {
				return err
			}
		}
		newCfg, qerr := encodeFleetConfig(s.cipher, cur.Backend, config)
		if qerr != nil {
			return qerr
		}
		row, qerr := q.UpdateProjectFleet(ctx, sqlcgen.UpdateProjectFleetParams{ProjectID: projectID, ID: id, Name: newName, Config: newCfg})
		if errors.Is(qerr, pgx.ErrNoRows) {
			return ErrFleetNotFound
		}
		if qerr != nil {
			return translateName(qerr)
		}
		f, ferr := fleetRowToModel(s.cipher, row)
		if ferr != nil {
			return ferr
		}
		out = f
		return actor.Audit(ctx, tx, tenantID, projectID, actionUpdate, strconv.FormatInt(id, 10), map[string]any{
			"fleet_name": f.Name, "backend": f.Backend, "config_keys": configKeys(f.Config),
		})
	})
	if err != nil {
		return nil, fmt.Errorf("fleet: update fleet: %w", err)
	}
	return out, nil
}

// DeleteFleet soft-deletes a live fleet of the project. Live allocations do
// not stop the delete. It returns ErrFleetNotFound when no live fleet matched.
func (s *PostgresFleetStore) DeleteFleet(ctx context.Context, tenantID, projectID, id int64, actor projectadmin.Actor) error {
	if s == nil {
		return errNoFleetStore
	}
	err := s.pool.Q(db.WithTenant(ctx, tenantID), func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).SoftDeleteProjectFleet(ctx, sqlcgen.SoftDeleteProjectFleetParams{ProjectID: projectID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFleetNotFound
		}
		if err != nil {
			return err
		}
		return actor.Audit(ctx, tx, tenantID, projectID, actionDelete, strconv.FormatInt(id, 10), map[string]any{
			"fleet_name": row.Name, "backend": row.Backend,
		})
	})
	if err != nil {
		return fmt.Errorf("fleet: delete fleet: %w", err)
	}
	return nil
}

// RestoreFleet undoes a delete: same id, name, backend, and config. It
// returns ErrFleetNotFound when no deleted fleet of the project matched, and
// ErrFleetNameTaken when a live fleet has the name now.
func (s *PostgresFleetStore) RestoreFleet(ctx context.Context, tenantID, projectID, id int64, actor projectadmin.Actor) (*Fleet, error) {
	if s == nil {
		return nil, errNoFleetStore
	}
	var out *Fleet
	err := s.pool.Q(db.WithTenant(ctx, tenantID), func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).RestoreProjectFleet(ctx, sqlcgen.RestoreProjectFleetParams{ProjectID: projectID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFleetNotFound
		}
		if err != nil {
			return translateName(err)
		}
		f, ferr := fleetRowToModel(s.cipher, row)
		if ferr != nil {
			return ferr
		}
		out = f
		return actor.Audit(ctx, tx, tenantID, projectID, actionRestore, strconv.FormatInt(id, 10), map[string]any{
			"fleet_name": f.Name, "backend": f.Backend,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("fleet: restore fleet: %w", err)
	}
	return out, nil
}

// ServerAllocate is the input of AllocateServer. The fleet is a live fleet
// of the project. A Capacity of 0 means 1.
type ServerAllocate struct {
	Fleet    *Fleet
	Region   string
	GameMode string
	Capacity int
}

// ValidText reports whether s is a valid name, backend, region, or game
// mode: valid UTF-8 with no NUL byte, at most MaxConfigKeyOrVal bytes.
func ValidText(s string) bool {
	return len(s) <= MaxConfigKeyOrVal && webutil.ValidText(s)
}

// validateAllocate checks the client input of AllocateServer. The messages
// name the field and never carry the value.
func validateAllocate(in ServerAllocate, maxCapacity int) error {
	switch {
	case !ValidText(in.Region):
		return InvalidError("Region is not valid or too long.")
	case !ValidText(in.GameMode):
		return InvalidError("Game mode is not valid or too long.")
	case in.Capacity < 0:
		return InvalidError("Capacity must be at least 1.")
	case maxCapacity > 0 && in.Capacity > maxCapacity:
		return InvalidError("Capacity is above the limit of this server.")
	}
	return nil
}

// AllocateServer starts a game server through the manager, with no
// authorization. The audit row is written in the transaction that marks the
// allocation ready: it commits with that change, and a failed audit write
// frees the server again. A failed backend call leaves no audit row.
func AllocateServer(ctx context.Context, m *Manager, tenantID int64, in ServerAllocate, actor projectadmin.Actor) (*Allocation, error) {
	if err := validateAllocate(in, m.opts.MaxCapacity); err != nil {
		return nil, err
	}
	if in.Capacity == 0 {
		in.Capacity = 1
	}
	ctx = WithCommitHook(db.WithTenant(ctx, tenantID), func(ctx context.Context, tx pgx.Tx, id AllocationID) error {
		return actor.Audit(ctx, tx, tenantID, in.Fleet.ProjectID, actionAllocate, strconv.FormatInt(int64(id), 10), map[string]any{
			"fleet_id":   in.Fleet.ID,
			"fleet_name": in.Fleet.Name,
			"region":     in.Region,
			"game_mode":  in.GameMode,
			"capacity":   in.Capacity,
		})
	})
	return m.Allocate(ctx, AllocationRequest{
		TenantID:  tenantID,
		ProjectID: in.Fleet.ProjectID,
		FleetID:   in.Fleet.ID,
		Region:    in.Region,
		GameMode:  in.GameMode,
		Capacity:  in.Capacity,
	})
}

// DeallocateServer stops the game server of an allocation of the project,
// with no authorization, and returns the allocation after the change. It
// returns ErrNotFound for an allocation of another project. The backend
// stops the server first; then the row is released and its audit row is
// written in one transaction, so a failed backend call changes nothing and
// can be retried. An allocation that is already released or failed is
// returned as it is, with no new audit row.
func DeallocateServer(ctx context.Context, m *Manager, tenantID, projectID int64, id AllocationID, actor projectadmin.Actor) (*Allocation, error) {
	ctx = db.WithTenant(ctx, tenantID)
	a, err := m.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.ProjectID != projectID {
		return nil, ErrNotFound
	}
	hooked := WithCommitHook(ctx, func(ctx context.Context, tx pgx.Tx, id AllocationID) error {
		return actor.Audit(ctx, tx, tenantID, projectID, actionDeallocate, strconv.FormatInt(int64(id), 10), nil)
	})
	if err := m.Deallocate(hooked, id); err != nil {
		return nil, err
	}
	return m.Get(ctx, id)
}
