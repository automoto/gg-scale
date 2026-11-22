package fleet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/projectadmin"
)

// Limits on a fleet config, for the dashboard form and the management API.
const (
	MaxConfigKeys     = 64
	MaxConfigKeyOrVal = 256
)

// Audit actions of the fleet writes. The dashboard and tokens use the same
// names, with different actors.
const (
	ActionCreate  = "fleet.template.create"
	ActionUpdate  = "fleet.template.update"
	ActionDelete  = "fleet.template.delete"
	ActionRestore = "fleet.template.restore"

	ActionAllocate   = "fleet.allocate.manual"
	ActionDeallocate = "fleet.deallocate.manual"
)

var (
	// ErrFleetNameTaken means a live fleet of the project has the name.
	ErrFleetNameTaken = errors.New("fleet: a fleet with that name already exists in this Game Project")
)

// InvalidError means the name or config is not valid. The text names the
// field and never the value, because a plugin config can hold a secret. The
// dashboard and the management API show it as it is.
type InvalidError string

func (e InvalidError) Error() string { return string(e) }

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
	return nil
}

func translateName(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrFleetNameTaken
	}
	return err
}

func configKeys(cfg map[string]string) []string {
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// CreateFleet inserts a fleet and its audit row in one transaction. It does
// no authorization. The audit payload has config keys, never values.
func CreateFleet(ctx context.Context, pool *db.Pool, tenantID int64, in FleetCreate, actor projectadmin.Actor) (*Fleet, error) {
	if err := validateName(in.Name); err != nil {
		return nil, err
	}
	if err := ValidateConfig(in.Backend, in.Config); err != nil {
		return nil, err
	}
	cfg, err := encodeFleetConfig(in.Config)
	if err != nil {
		return nil, err
	}
	var out *Fleet
	err = pool.Q(db.WithTenant(ctx, tenantID), func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).CreateFleet(ctx, sqlcgen.CreateFleetParams{
			ProjectID: in.ProjectID, Name: in.Name, Backend: in.Backend, Config: cfg,
		})
		if qerr != nil {
			return translateName(qerr)
		}
		f, ferr := fleetRowToModel(row)
		if ferr != nil {
			return ferr
		}
		out = f
		return actor.Audit(ctx, tx, tenantID, in.ProjectID, ActionCreate, strconv.FormatInt(f.ID, 10), map[string]any{
			"fleet_name": f.Name, "backend": f.Backend, "config_keys": configKeys(in.Config),
		})
	})
	if err != nil {
		return nil, fmt.Errorf("fleet: create fleet: %w", err)
	}
	return out, nil
}

// UpdateFleet changes the name and config of a live fleet of the project. A
// nil name or config keeps the stored value. The backend is fixed. It returns
// ErrFleetNotFound for a deleted fleet or a fleet of another project.
func UpdateFleet(ctx context.Context, pool *db.Pool, tenantID, projectID, id int64, name *string, config map[string]string, actor projectadmin.Actor) (*Fleet, error) {
	if name != nil {
		if err := validateName(*name); err != nil {
			return nil, err
		}
	}
	var out *Fleet
	err := pool.Q(db.WithTenant(ctx, tenantID), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		cur, qerr := q.GetProjectFleet(ctx, sqlcgen.GetProjectFleetParams{ProjectID: projectID, ID: id})
		if errors.Is(qerr, pgx.ErrNoRows) || (qerr == nil && cur.DeletedAt.Valid) {
			return ErrFleetNotFound
		}
		if qerr != nil {
			return qerr
		}
		newName, newCfg := cur.Name, cur.Config
		if name != nil {
			newName = *name
		}
		if config != nil {
			if err := ValidateConfig(cur.Backend, config); err != nil {
				return err
			}
			if newCfg, qerr = encodeFleetConfig(config); qerr != nil {
				return qerr
			}
		}
		row, qerr := q.UpdateProjectFleet(ctx, sqlcgen.UpdateProjectFleetParams{ProjectID: projectID, ID: id, Name: newName, Config: newCfg})
		if errors.Is(qerr, pgx.ErrNoRows) {
			return ErrFleetNotFound
		}
		if qerr != nil {
			return translateName(qerr)
		}
		f, ferr := fleetRowToModel(row)
		if ferr != nil {
			return ferr
		}
		out = f
		return actor.Audit(ctx, tx, tenantID, projectID, ActionUpdate, strconv.FormatInt(id, 10), map[string]any{
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
func DeleteFleet(ctx context.Context, pool *db.Pool, tenantID, projectID, id int64, actor projectadmin.Actor) error {
	err := pool.Q(db.WithTenant(ctx, tenantID), func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).SoftDeleteProjectFleet(ctx, sqlcgen.SoftDeleteProjectFleetParams{ProjectID: projectID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFleetNotFound
		}
		if err != nil {
			return err
		}
		return actor.Audit(ctx, tx, tenantID, projectID, ActionDelete, strconv.FormatInt(id, 10), map[string]any{
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
func RestoreFleet(ctx context.Context, pool *db.Pool, tenantID, projectID, id int64, actor projectadmin.Actor) (*Fleet, error) {
	var out *Fleet
	err := pool.Q(db.WithTenant(ctx, tenantID), func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).RestoreProjectFleet(ctx, sqlcgen.RestoreProjectFleetParams{ProjectID: projectID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFleetNotFound
		}
		if err != nil {
			return translateName(err)
		}
		f, ferr := fleetRowToModel(row)
		if ferr != nil {
			return ferr
		}
		out = f
		return actor.Audit(ctx, tx, tenantID, projectID, ActionRestore, strconv.FormatInt(id, 10), map[string]any{
			"fleet_name": f.Name, "backend": f.Backend,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("fleet: restore fleet: %w", err)
	}
	return out, nil
}

// ServerAllocate is the input of AllocateServer. The fleet is a live fleet
// of the project. A Capacity below 1 means 1.
type ServerAllocate struct {
	Fleet    *Fleet
	Region   string
	GameMode string
	Capacity int
}

// AllocateServer starts a game server through the manager, with no
// authorization. The audit row is written in the transaction that marks the
// allocation ready: it commits with that change, and a failed audit write
// frees the server again. A failed backend call leaves no audit row.
func AllocateServer(ctx context.Context, m *Manager, tenantID int64, in ServerAllocate, actor projectadmin.Actor) (*Allocation, error) {
	if in.Capacity < 1 {
		in.Capacity = 1
	}
	ctx = WithCommitHook(db.WithTenant(ctx, tenantID), func(ctx context.Context, tx pgx.Tx, id AllocationID) error {
		return actor.Audit(ctx, tx, tenantID, in.Fleet.ProjectID, ActionAllocate, strconv.FormatInt(int64(id), 10), map[string]any{
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
		return actor.Audit(ctx, tx, tenantID, projectID, ActionDeallocate, strconv.FormatInt(int64(id), 10), nil)
	})
	if err := m.Deallocate(hooked, id); err != nil {
		return nil, err
	}
	return m.Get(ctx, id)
}
