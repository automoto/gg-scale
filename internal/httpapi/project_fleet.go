package httpapi

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/fleet"
	"github.com/automoto/gg-scale/internal/managementtoken"
)

// allocationEventRing is the number of events that the database keeps for
// one allocation. One read returns all of them.
const allocationEventRing = 50

// projectFleet is a fleet template as the dashboard shows it. config is
// returned by get_fleet for the agones backend only.
type projectFleet struct {
	ID        int64             `json:"id" example:"3"`
	Name      string            `json:"name" example:"default"`
	Backend   string            `json:"backend" example:"agones"`
	Config    map[string]string `json:"config,omitempty" doc:"The settings of an agones fleet. Only in get_fleet. Left out for other backends, whose settings may hold secrets."`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// fleetConfig returns the config that a token may see: only the agones
// backend, whose keys are known and not secret. A plugin config is free-form
// and may hold a secret.
func fleetConfig(f *fleet.Fleet) map[string]string {
	if f.Backend != "agones" {
		return nil
	}
	return f.Config
}

func newProjectFleet(f *fleet.Fleet, withConfig bool) projectFleet {
	out := projectFleet{ID: f.ID, Name: f.Name, Backend: f.Backend, CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt}
	if withConfig {
		out.Config = fleetConfig(f)
	}
	return out
}

type projectFleetList struct {
	Fleets     []projectFleet `json:"fleets"`
	NextCursor string         `json:"next_cursor,omitempty" doc:"Send as cursor for the next page. Absent on the last page."`
}

// fleetManager returns the fleet manager, or a 503 when the server has no
// fleet backend. The fleet rows are registered only with the fleet switch on,
// but the backend can still be missing.
func fleetManager(d Deps) (*fleet.Manager, error) {
	if d.Fleet == nil {
		return nil, huma.Error503ServiceUnavailable("fleet backend not configured")
	}
	return d.Fleet, nil
}

// listFleets pages by name: the cursor is the name of the last fleet of the
// page. A project has few fleets, so one read gets all of them.
func listFleets(ctx context.Context, d Deps, p managementtoken.Principal, in *ProjectPageInput) (projectFleetList, error) {
	m, err := fleetManager(d)
	if err != nil {
		return projectFleetList{}, err
	}
	all, err := m.Fleets().ListForProject(db.WithTenant(ctx, p.TenantID), p.ProjectID)
	if err != nil {
		return projectFleetList{}, err
	}
	limit := int(in.limit())
	out := projectFleetList{Fleets: []projectFleet{}}
	for _, f := range all {
		if f.Name <= in.Cursor {
			continue
		}
		if len(out.Fleets) == limit {
			out.NextCursor = out.Fleets[limit-1].Name
			break
		}
		out.Fleets = append(out.Fleets, newProjectFleet(f, false))
	}
	return out, nil
}

type projectFleetInput struct {
	FleetID int64 `path:"fleet_id" minimum:"1" doc:"The fleet ID."`
}

func getFleet(ctx context.Context, d Deps, p managementtoken.Principal, in *projectFleetInput) (projectFleet, error) {
	m, err := fleetManager(d)
	if err != nil {
		return projectFleet{}, err
	}
	f, err := m.Fleets().GetByID(db.WithTenant(ctx, p.TenantID), in.FleetID)
	if errors.Is(err, fleet.ErrFleetNotFound) {
		return projectFleet{}, errProjectNotFound()
	}
	if err != nil {
		return projectFleet{}, err
	}
	if f.ProjectID != p.ProjectID {
		return projectFleet{}, errProjectNotFound()
	}
	return newProjectFleet(f, true), nil
}

// projectAllocation is a game server slot as the dashboard shows it. It has
// no labels, join data, or session data. address is the host and port of
// the server, which the matchmaker hands to matched players: it is not a
// credential.
type projectAllocation struct {
	ID         int64  `json:"id" example:"17"`
	FleetID    int64  `json:"fleet_id" example:"3"`
	Backend    string `json:"backend" example:"agones"`
	BackendRef string `json:"backend_ref" doc:"The backend name of the server, for example the Agones GameServer name."`
	Region     string `json:"region" example:"eu"`
	Address    string `json:"address" example:"203.0.113.5:7777"`
	Status     string `json:"status" example:"ready"`
}

func newProjectAllocation(a *fleet.Allocation) projectAllocation {
	return projectAllocation{
		ID: int64(a.ID), FleetID: a.FleetID, Backend: a.Backend, BackendRef: a.BackendRef,
		Region: a.Region, Address: a.Address, Status: a.Status.String(),
	}
}

type projectAllocationList struct {
	Allocations []projectAllocation `json:"allocations"`
	NextCursor  string              `json:"next_cursor,omitempty" doc:"Send as cursor for the next page. Absent on the last page."`
}

type projectListAllocationsInput struct {
	ProjectPageInput
	IncludeTerminal bool `query:"include_terminal" doc:"Also list finished allocations (shutdown and failed). The default is live ones only."`
}

// decodeIDCursor reads a cursor that is a row ID: empty, or a positive number.
func decodeIDCursor(s string) (int64, bool) {
	if s == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// listAllocations pages newest first by allocation ID: the cursor is the ID
// of the last allocation of the page.
func listAllocations(ctx context.Context, d Deps, p managementtoken.Principal, in *projectListAllocationsInput) (projectAllocationList, error) {
	if _, err := fleetManager(d); err != nil {
		return projectAllocationList{}, err
	}
	before, ok := decodeIDCursor(in.Cursor)
	if !ok {
		return projectAllocationList{}, huma.Error422UnprocessableEntity("cursor is not valid")
	}
	limit := in.limit()
	var rows []sqlcgen.ListAllocationsPageForProjectRow
	err := projectQ(ctx, d, p, func(ctx context.Context, q *sqlcgen.Queries) error {
		var err error
		rows, err = q.ListAllocationsPageForProject(ctx, sqlcgen.ListAllocationsPageForProjectParams{
			ProjectID: p.ProjectID, IncludeTerminal: in.IncludeTerminal, BeforeID: before, RowLimit: limit + 1,
		})
		return err
	})
	if err != nil {
		return projectAllocationList{}, err
	}
	rows, more := trimPage(rows, limit)
	out := projectAllocationList{Allocations: make([]projectAllocation, 0, len(rows))}
	for _, r := range rows {
		out.Allocations = append(out.Allocations, projectAllocation{
			ID: r.ID, FleetID: derefInt64(r.FleetID), Backend: r.Backend, BackendRef: r.BackendRef,
			Region: r.Region, Address: r.Address, Status: r.Status,
		})
	}
	if more {
		out.NextCursor = strconv.FormatInt(rows[len(rows)-1].ID, 10)
	}
	return out, nil
}

type projectAllocationInput struct {
	AllocationID int64 `path:"allocation_id" minimum:"1" doc:"The allocation ID."`
}

// ownAllocation returns the allocation, or a 404 when it is not one of this
// project.
func ownAllocation(ctx context.Context, d Deps, p managementtoken.Principal, id int64) (context.Context, *fleet.Manager, *fleet.Allocation, error) {
	m, err := fleetManager(d)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx = db.WithTenant(ctx, p.TenantID)
	a, err := m.Get(ctx, fleet.AllocationID(id))
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, nil, nil, errProjectNotFound()
	}
	if err != nil {
		return nil, nil, nil, err
	}
	if a.ProjectID != p.ProjectID {
		return nil, nil, nil, errProjectNotFound()
	}
	return ctx, m, a, nil
}

func getAllocation(ctx context.Context, d Deps, p managementtoken.Principal, in *projectAllocationInput) (projectAllocation, error) {
	_, _, a, err := ownAllocation(ctx, d, p, in.AllocationID)
	if err != nil {
		return projectAllocation{}, err
	}
	return newProjectAllocation(a), nil
}

type projectAllocationEvent struct {
	ID        int64     `json:"id"`
	Status    string    `json:"status" example:"ready"`
	Address   string    `json:"address"`
	Error     string    `json:"error" doc:"The error text of the backend. Empty when there was no error."`
	CreatedAt time.Time `json:"created_at"`
}

type projectAllocationEventList struct {
	Events     []projectAllocationEvent `json:"events"`
	NextCursor string                   `json:"next_cursor,omitempty" doc:"Send as cursor for the next page. Absent on the last page."`
}

type projectListAllocationEventsInput struct {
	AllocationID int64 `path:"allocation_id" minimum:"1" doc:"The allocation ID."`
	ProjectPageInput
}

// listAllocationEvents pages newest first. The database keeps at most
// allocationEventRing events for an allocation, so one read gets all of them.
// The cursor is the ID of the last event of the page.
func listAllocationEvents(ctx context.Context, d Deps, p managementtoken.Principal, in *projectListAllocationEventsInput) (projectAllocationEventList, error) {
	ctx, m, _, err := ownAllocation(ctx, d, p, in.AllocationID)
	if err != nil {
		return projectAllocationEventList{}, err
	}
	before := parseCursor(in.Cursor)
	all, err := m.ListEvents(ctx, fleet.AllocationID(in.AllocationID), allocationEventRing)
	if err != nil {
		return projectAllocationEventList{}, err
	}
	limit := int(in.limit())
	out := projectAllocationEventList{Events: []projectAllocationEvent{}}
	for _, e := range all {
		if before > 0 && e.ID >= before {
			continue
		}
		if len(out.Events) == limit {
			out.NextCursor = strconv.FormatInt(out.Events[limit-1].ID, 10)
			break
		}
		out.Events = append(out.Events, projectAllocationEvent{
			ID: e.ID, Status: e.Status.String(), Address: e.Address, Error: e.ErrMessage, CreatedAt: e.CreatedAt,
		})
	}
	return out, nil
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
