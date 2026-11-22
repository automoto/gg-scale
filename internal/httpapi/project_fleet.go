package httpapi

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/fleet"
	"github.com/automoto/gg-scale/internal/managementtoken"
	"github.com/automoto/gg-scale/internal/rbac"
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

// newProjectFleet shows the config, when withConfig is set, of the agones
// backend only: its keys are known and not secret. A plugin config is
// free-form and may hold a secret.
func newProjectFleet(f *fleet.Fleet, withConfig bool) projectFleet {
	out := projectFleet{ID: f.ID, Name: f.Name, Backend: f.Backend, CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt}
	if withConfig && f.Backend == "agones" {
		out.Config = f.Config
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
// page. A project has few fleets, so one read gets all of them. The database
// sorts by its collation, so the list is sorted again in byte order, the
// order of the cursor compare.
func listFleets(ctx context.Context, d Deps, p managementtoken.Principal, in *ProjectPageInput) (projectFleetList, error) {
	m, err := fleetManager(d)
	if err != nil {
		return projectFleetList{}, err
	}
	all, err := m.Fleets().ListForProject(db.WithTenant(ctx, p.TenantID), p.ProjectID)
	if err != nil {
		return projectFleetList{}, err
	}
	slices.SortFunc(all, func(a, b *fleet.Fleet) int { return strings.Compare(a.Name, b.Name) })
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

// projectFleetByID returns the fleet (live or deleted) of this project, or
// answers 404.
func projectFleetByID(ctx context.Context, d Deps, p managementtoken.Principal, id int64) (*fleet.Fleet, error) {
	m, err := fleetManager(d)
	if err != nil {
		return nil, err
	}
	f, err := m.Fleets().GetByID(db.WithTenant(ctx, p.TenantID), id)
	if errors.Is(err, fleet.ErrFleetNotFound) {
		return nil, errProjectNotFound()
	}
	if err != nil {
		return nil, err
	}
	if f.ProjectID != p.ProjectID {
		return nil, errProjectNotFound()
	}
	return f, nil
}

func getFleet(ctx context.Context, d Deps, p managementtoken.Principal, in *projectFleetInput) (projectFleet, error) {
	f, err := projectFleetByID(ctx, d, p, in.FleetID)
	if err != nil {
		return projectFleet{}, err
	}
	if f.DeletedAt != nil {
		return projectFleet{}, errProjectNotFound()
	}
	return newProjectFleet(f, true), nil
}

// fleetWriteError maps the errors of the fleet writes to safe client errors.
// The texts never carry a config value. Other errors are returned unchanged
// and become a 500.
func fleetWriteError(err error) error {
	var invalid fleet.InvalidError
	switch {
	case errors.Is(err, fleet.ErrFleetNotFound):
		return errProjectNotFound()
	case errors.Is(err, fleet.ErrFleetNameTaken):
		return huma.Error409Conflict("a fleet with that name already exists in this Game Project")
	case errors.As(err, &invalid):
		return huma.Error422UnprocessableEntity(invalid.Error())
	}
	return err
}

// requireBackendFeature refuses the request when the tenant has no feature
// grant for the fleet backend, as the dashboard does for each fleet write.
func requireBackendFeature(ctx context.Context, d Deps, p managementtoken.Principal, backend string) error {
	feature, ok := rbac.BackendFeature(backend)
	if !ok {
		return huma.Error422UnprocessableEntity("backend must be agones or plugin:<name>")
	}
	on, err := d.RBAC.FeatureEnabled(ctx, p.TenantID, p.ProjectID, feature)
	if err != nil {
		return serverError(ctx, "project management: feature check", err)
	}
	if !on {
		return huma.Error403Forbidden("This fleet backend is not enabled for this Account Tenant.")
	}
	return nil
}

// ownFleet returns the fleet (live or deleted) of this project and checks
// the grant of its backend, or answers 404.
func ownFleet(ctx context.Context, d Deps, p managementtoken.Principal, id int64) (*fleet.Fleet, error) {
	f, err := projectFleetByID(ctx, d, p, id)
	if err != nil {
		return nil, err
	}
	if err := requireBackendFeature(ctx, d, p, f.Backend); err != nil {
		return nil, err
	}
	return f, nil
}

type projectCreateFleetBody struct {
	Name    string            `json:"name" example:"ranked"`
	Backend string            `json:"backend" example:"agones" doc:"agones or plugin:<name>."`
	Config  map[string]string `json:"config,omitempty" doc:"agones: fleet_name (required), namespace, and selector.<label> keys. A plugin backend takes its own keys."`
}

type projectCreateFleetInput struct {
	Body projectCreateFleetBody
}

func createFleet(ctx context.Context, d Deps, p managementtoken.Principal, in *projectCreateFleetInput) (projectFleet, error) {
	if _, err := fleetManager(d); err != nil {
		return projectFleet{}, err
	}
	if err := requireBackendFeature(ctx, d, p, in.Body.Backend); err != nil {
		return projectFleet{}, err
	}
	f, err := fleet.CreateFleet(ctx, d.Pool, p.TenantID, fleet.FleetCreate{
		ProjectID: p.ProjectID, Name: in.Body.Name, Backend: in.Body.Backend, Config: in.Body.Config,
	}, projectActor(p))
	if err != nil {
		return projectFleet{}, fleetWriteError(err)
	}
	return newProjectFleet(f, true), nil
}

type projectUpdateFleetBody struct {
	Name   *string           `json:"name,omitempty" doc:"The new name. Left out keeps the name."`
	Config map[string]string `json:"config,omitempty" doc:"The full new config; it replaces the old one. Left out keeps the config. The backend cannot change."`
}

type projectUpdateFleetInput struct {
	FleetID int64 `path:"fleet_id" minimum:"1" doc:"The fleet ID."`
	Body    projectUpdateFleetBody
}

func updateFleet(ctx context.Context, d Deps, p managementtoken.Principal, in *projectUpdateFleetInput) (projectFleet, error) {
	if _, err := ownFleet(ctx, d, p, in.FleetID); err != nil {
		return projectFleet{}, err
	}
	f, err := fleet.UpdateFleet(ctx, d.Pool, p.TenantID, p.ProjectID, in.FleetID, in.Body.Name, in.Body.Config, projectActor(p))
	if err != nil {
		return projectFleet{}, fleetWriteError(err)
	}
	return newProjectFleet(f, true), nil
}

func deleteFleet(ctx context.Context, d Deps, p managementtoken.Principal, in *projectFleetInput) (projectDeletedResult, error) {
	if _, err := ownFleet(ctx, d, p, in.FleetID); err != nil {
		return projectDeletedResult{}, err
	}
	if err := fleet.DeleteFleet(ctx, d.Pool, p.TenantID, p.ProjectID, in.FleetID, projectActor(p)); err != nil {
		return projectDeletedResult{}, fleetWriteError(err)
	}
	return projectDeletedResult{Deleted: true, Note: "Allocations are kept. Use restore_fleet to undo."}, nil
}

func restoreFleet(ctx context.Context, d Deps, p managementtoken.Principal, in *projectFleetInput) (projectFleet, error) {
	if _, err := ownFleet(ctx, d, p, in.FleetID); err != nil {
		return projectFleet{}, err
	}
	f, err := fleet.RestoreFleet(ctx, d.Pool, p.TenantID, p.ProjectID, in.FleetID, projectActor(p))
	if err != nil {
		return projectFleet{}, fleetWriteError(err)
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
	Region     string `json:"region" example:"eu" doc:"Shown only when it is a short identifier; other text is replaced by a marker with its length."`
	Address    string `json:"address" example:"203.0.113.5:7777"`
	Status     string `json:"status" example:"ready"`
}

func newProjectAllocation(a *fleet.Allocation) projectAllocation {
	return projectAllocation{
		ID: int64(a.ID), FleetID: a.FleetID, Backend: a.Backend, BackendRef: a.BackendRef,
		Region: clientText(a.Region), Address: a.Address, Status: a.Status.String(),
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

// listAllocations pages newest first by allocation ID: the cursor is the ID
// of the last allocation of the page.
func listAllocations(ctx context.Context, d Deps, p managementtoken.Principal, in *projectListAllocationsInput) (projectAllocationList, error) {
	if _, err := fleetManager(d); err != nil {
		return projectAllocationList{}, err
	}
	before, ok := decodeIDCursor(in.Cursor)
	if !ok {
		return projectAllocationList{}, errBadCursor()
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
			Region: clientText(r.Region), Address: r.Address, Status: r.Status,
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
func ownAllocation(ctx context.Context, d Deps, p managementtoken.Principal, id int64) (*fleet.Allocation, error) {
	m, err := fleetManager(d)
	if err != nil {
		return nil, err
	}
	a, err := m.Get(db.WithTenant(ctx, p.TenantID), fleet.AllocationID(id))
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, errProjectNotFound()
	}
	if err != nil {
		return nil, err
	}
	if a.ProjectID != p.ProjectID {
		return nil, errProjectNotFound()
	}
	return a, nil
}

func getAllocation(ctx context.Context, d Deps, p managementtoken.Principal, in *projectAllocationInput) (projectAllocation, error) {
	a, err := ownAllocation(ctx, d, p, in.AllocationID)
	if err != nil {
		return projectAllocation{}, err
	}
	return newProjectAllocation(a), nil
}

type projectAllocateBody struct {
	Fleet    string `json:"fleet" minLength:"1" example:"ranked" doc:"The name of a live fleet of this Game Project."`
	Region   string `json:"region" minLength:"1" example:"eu" doc:"The region of the server."`
	GameMode string `json:"game_mode,omitempty" example:"duel" doc:"The game mode. Optional."`
	Capacity int    `json:"capacity,omitempty" minimum:"1" example:"2" doc:"The player capacity. The default is 1."`
}

type projectAllocateInput struct {
	Body projectAllocateBody
}

// allocateServer starts a server as the dashboard does. A deleted fleet
// answers 404 like a missing one. The caller cannot set labels or join data.
func allocateServer(ctx context.Context, d Deps, p managementtoken.Principal, in *projectAllocateInput) (projectAllocation, error) {
	region := strings.TrimSpace(in.Body.Region)
	name := strings.TrimSpace(in.Body.Fleet)
	if region == "" || name == "" {
		return projectAllocation{}, huma.Error422UnprocessableEntity("fleet and region are required")
	}
	if !fleet.ValidText(name) {
		return projectAllocation{}, huma.Error422UnprocessableEntity("fleet is not valid or too long")
	}
	m, err := fleetManager(d)
	if err != nil {
		return projectAllocation{}, err
	}
	f, err := m.Fleets().GetByName(db.WithTenant(ctx, p.TenantID), p.ProjectID, name)
	if errors.Is(err, fleet.ErrFleetNotFound) {
		return projectAllocation{}, errProjectNotFound()
	}
	if err != nil {
		return projectAllocation{}, err
	}
	if err := requireBackendFeature(ctx, d, p, f.Backend); err != nil {
		return projectAllocation{}, err
	}
	a, err := fleet.AllocateServer(ctx, m, p.TenantID, fleet.ServerAllocate{
		Fleet: f, Region: region, GameMode: strings.TrimSpace(in.Body.GameMode), Capacity: in.Body.Capacity,
	}, projectActor(p))
	if errors.Is(err, fleet.ErrFleetBackendMismatch) {
		return projectAllocation{}, huma.Error409Conflict("the fleet backend is not the backend of this server")
	}
	if err != nil {
		return projectAllocation{}, fleetWriteError(err)
	}
	return newProjectAllocation(a), nil
}

// deallocateServer stops a server as the dashboard does. An allocation that
// is already released is returned as it is, with no new audit row.
func deallocateServer(ctx context.Context, d Deps, p managementtoken.Principal, in *projectAllocationInput) (projectAllocation, error) {
	m, err := fleetManager(d)
	if err != nil {
		return projectAllocation{}, err
	}
	a, err := fleet.DeallocateServer(ctx, m, p.TenantID, p.ProjectID, fleet.AllocationID(in.AllocationID), projectActor(p))
	if errors.Is(err, fleet.ErrNotFound) {
		return projectAllocation{}, errProjectNotFound()
	}
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
	if _, err := ownAllocation(ctx, d, p, in.AllocationID); err != nil {
		return projectAllocationEventList{}, err
	}
	before, ok := decodeIDCursor(in.Cursor)
	if !ok {
		return projectAllocationEventList{}, errBadCursor()
	}
	all, err := d.Fleet.ListEvents(db.WithTenant(ctx, p.TenantID), fleet.AllocationID(in.AllocationID), allocationEventRing)
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
