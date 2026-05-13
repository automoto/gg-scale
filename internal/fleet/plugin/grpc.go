package plugin

import (
	"context"
	"errors"
	"math"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ggscale/ggscale/internal/fleet"
	fleetpb "github.com/ggscale/ggscale/internal/fleet/plugin/proto"
)

func statusToProto(s fleet.Status) fleetpb.AllocationStatus {
	switch s {
	case fleet.StatusPending:
		return fleetpb.AllocationStatus_ALLOCATION_STATUS_PENDING
	case fleet.StatusAllocating:
		return fleetpb.AllocationStatus_ALLOCATION_STATUS_ALLOCATING
	case fleet.StatusReady:
		return fleetpb.AllocationStatus_ALLOCATION_STATUS_READY
	case fleet.StatusAllocated:
		return fleetpb.AllocationStatus_ALLOCATION_STATUS_ALLOCATED
	case fleet.StatusDraining:
		return fleetpb.AllocationStatus_ALLOCATION_STATUS_DRAINING
	case fleet.StatusShutdown:
		return fleetpb.AllocationStatus_ALLOCATION_STATUS_SHUTDOWN
	case fleet.StatusFailed:
		return fleetpb.AllocationStatus_ALLOCATION_STATUS_FAILED
	default:
		return fleetpb.AllocationStatus_ALLOCATION_STATUS_UNSPECIFIED
	}
}

func statusFromProto(s fleetpb.AllocationStatus) fleet.Status {
	switch s {
	case fleetpb.AllocationStatus_ALLOCATION_STATUS_PENDING:
		return fleet.StatusPending
	case fleetpb.AllocationStatus_ALLOCATION_STATUS_ALLOCATING:
		return fleet.StatusAllocating
	case fleetpb.AllocationStatus_ALLOCATION_STATUS_READY:
		return fleet.StatusReady
	case fleetpb.AllocationStatus_ALLOCATION_STATUS_ALLOCATED:
		return fleet.StatusAllocated
	case fleetpb.AllocationStatus_ALLOCATION_STATUS_DRAINING:
		return fleet.StatusDraining
	case fleetpb.AllocationStatus_ALLOCATION_STATUS_SHUTDOWN:
		return fleet.StatusShutdown
	case fleetpb.AllocationStatus_ALLOCATION_STATUS_FAILED:
		return fleet.StatusFailed
	default:
		// Treat UNSPECIFIED / unknown as a terminal failure so a misbehaving
		// plugin cannot stall the manager.
		return fleet.StatusFailed
	}
}

func reqToProto(r fleet.AllocationRequest) *fleetpb.AllocationRequest {
	capacity := r.Capacity
	if capacity < 0 || capacity > math.MaxInt32 {
		// Anything outside int32 would silently wrap on the proto field —
		// clamp to 0 so a config bug doesn't become an over-allocation.
		capacity = 0
	}
	return &fleetpb.AllocationRequest{
		TenantId:  r.TenantID,
		ProjectId: r.ProjectID,
		Region:    r.Region,
		GameMode:  r.GameMode,
		Capacity:  int32(capacity),
		Labels:    r.Labels,
	}
}

func reqFromProto(p *fleetpb.AllocationRequest) fleet.AllocationRequest {
	if p == nil {
		return fleet.AllocationRequest{}
	}
	return fleet.AllocationRequest{
		TenantID:  p.GetTenantId(),
		ProjectID: p.GetProjectId(),
		Region:    p.GetRegion(),
		GameMode:  p.GetGameMode(),
		Capacity:  int(p.GetCapacity()),
		Labels:    p.GetLabels(),
	}
}

func allocToProto(a *fleet.Allocation) *fleetpb.Allocation {
	if a == nil {
		return nil
	}
	return &fleetpb.Allocation{
		Id:         int64(a.ID),
		TenantId:   a.TenantID,
		ProjectId:  a.ProjectID,
		Backend:    a.Backend,
		BackendRef: a.BackendRef,
		Region:     a.Region,
		Address:    a.Address,
		Status:     statusToProto(a.Status),
		Metadata:   a.Metadata,
	}
}

func allocFromProto(p *fleetpb.Allocation) *fleet.Allocation {
	if p == nil {
		return nil
	}
	return &fleet.Allocation{
		ID:         fleet.AllocationID(p.GetId()),
		TenantID:   p.GetTenantId(),
		ProjectID:  p.GetProjectId(),
		Backend:    p.GetBackend(),
		BackendRef: p.GetBackendRef(),
		Region:     p.GetRegion(),
		Address:    p.GetAddress(),
		Status:     statusFromProto(p.GetStatus()),
		Metadata:   p.GetMetadata(),
	}
}

// errToStatus / errFromStatus tunnel sentinel errors through gRPC status
// codes so the manager can re-queue (NotFound) or fall back (Unimplemented)
// without string-matching the message.

func errToStatus(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, fleet.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, fleet.ErrUnsupported):
		return status.Error(codes.Unimplemented, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func errFromStatus(err error) error {
	if err == nil {
		return nil
	}
	s, ok := status.FromError(err)
	if !ok {
		return err
	}
	switch s.Code() {
	case codes.OK:
		return nil
	case codes.NotFound:
		return fleet.ErrNotFound
	case codes.Unimplemented:
		return fleet.ErrUnsupported
	default:
		return errors.New(s.Message())
	}
}

// grpcServer runs inside the plugin subprocess and forwards inbound RPCs to a
// real fleet.Backend.

type grpcServer struct {
	fleetpb.UnimplementedFleetBackendServer
	impl fleet.Backend
}

func newGRPCServer(impl fleet.Backend) *grpcServer {
	return &grpcServer{impl: impl}
}

func (s *grpcServer) Name(_ context.Context, _ *fleetpb.NameRequest) (*fleetpb.NameResponse, error) {
	return &fleetpb.NameResponse{Name: s.impl.Name()}, nil
}

func (s *grpcServer) Allocate(ctx context.Context, req *fleetpb.AllocateRequest) (*fleetpb.AllocateResponse, error) {
	a, err := s.impl.Allocate(ctx, reqFromProto(req.GetRequest()))
	if err != nil {
		return nil, errToStatus(err)
	}
	return &fleetpb.AllocateResponse{Allocation: allocToProto(a)}, nil
}

func (s *grpcServer) Deallocate(ctx context.Context, req *fleetpb.DeallocateRequest) (*fleetpb.DeallocateResponse, error) {
	if err := s.impl.Deallocate(ctx, fleet.AllocationID(req.GetAllocationId()), req.GetBackendRef()); err != nil {
		return nil, errToStatus(err)
	}
	return &fleetpb.DeallocateResponse{}, nil
}

func (s *grpcServer) Status(ctx context.Context, req *fleetpb.StatusRequest) (*fleetpb.StatusResponse, error) {
	st, err := s.impl.Status(ctx, fleet.AllocationID(req.GetAllocationId()), req.GetBackendRef())
	if err != nil {
		return nil, errToStatus(err)
	}
	return &fleetpb.StatusResponse{Status: statusToProto(st)}, nil
}

func (s *grpcServer) Watch(req *fleetpb.WatchRequest, stream fleetpb.FleetBackend_WatchServer) error {
	ch, err := s.impl.Watch(stream.Context(), fleet.AllocationID(req.GetAllocationId()), req.GetBackendRef())
	if err != nil {
		return errToStatus(err)
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case upd, ok := <-ch:
			if !ok {
				return nil
			}
			msg := &fleetpb.StatusUpdate{
				Status:  statusToProto(upd.Status),
				Address: upd.Address,
			}
			if upd.Err != nil {
				msg.ErrorMessage = upd.Err.Error()
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

func (s *grpcServer) HealthCheck(ctx context.Context, _ *fleetpb.HealthCheckRequest) (*fleetpb.HealthCheckResponse, error) {
	if err := s.impl.HealthCheck(ctx); err != nil {
		return nil, errToStatus(err)
	}
	return &fleetpb.HealthCheckResponse{}, nil
}

func (s *grpcServer) Ping(ctx context.Context, _ *fleetpb.PingRequest) (*fleetpb.PingResponse, error) {
	if p, ok := s.impl.(pinger); ok {
		if err := p.Ping(ctx); err != nil {
			return nil, errToStatus(err)
		}
	}
	return &fleetpb.PingResponse{}, nil
}

// grpcClient implements fleet.Backend on the host side. Name is cached after
// the first call so the manager doesn't pay a round-trip every touch.
type grpcClient struct {
	client     fleetpb.FleetBackendClient
	ctx        context.Context // plugin lifetime; per-call deadlines come from the method ctx
	cachedName string
}

func newGRPCClient(ctx context.Context, c fleetpb.FleetBackendClient) *grpcClient {
	return &grpcClient{client: c, ctx: ctx}
}

func (c *grpcClient) Name() string {
	if c.cachedName != "" {
		return c.cachedName
	}
	resp, err := c.client.Name(c.ctx, &fleetpb.NameRequest{})
	if err != nil {
		return "plugin"
	}
	c.cachedName = resp.GetName()
	return c.cachedName
}

func (c *grpcClient) Allocate(ctx context.Context, req fleet.AllocationRequest) (*fleet.Allocation, error) {
	resp, err := c.client.Allocate(ctx, &fleetpb.AllocateRequest{Request: reqToProto(req)})
	if err != nil {
		return nil, errFromStatus(err)
	}
	return allocFromProto(resp.GetAllocation()), nil
}

func (c *grpcClient) Deallocate(ctx context.Context, id fleet.AllocationID, backendRef string) error {
	_, err := c.client.Deallocate(ctx, &fleetpb.DeallocateRequest{AllocationId: int64(id), BackendRef: backendRef})
	return errFromStatus(err)
}

func (c *grpcClient) Status(ctx context.Context, id fleet.AllocationID, backendRef string) (fleet.Status, error) {
	resp, err := c.client.Status(ctx, &fleetpb.StatusRequest{AllocationId: int64(id), BackendRef: backendRef})
	if err != nil {
		return fleet.StatusFailed, errFromStatus(err)
	}
	return statusFromProto(resp.GetStatus()), nil
}

func (c *grpcClient) Watch(ctx context.Context, id fleet.AllocationID, backendRef string) (<-chan fleet.StatusUpdate, error) {
	stream, err := c.client.Watch(ctx, &fleetpb.WatchRequest{AllocationId: int64(id), BackendRef: backendRef})
	if err != nil {
		return nil, errFromStatus(err)
	}
	out := make(chan fleet.StatusUpdate, 1)
	go func() {
		defer close(out)
		for {
			msg, err := stream.Recv()
			if err != nil {
				// Stream close (clean EOF or transport error) is the
				// manager's "no further updates" signal — surfacing
				// transport errors here would break that contract.
				return
			}
			upd := fleet.StatusUpdate{
				Status:  statusFromProto(msg.GetStatus()),
				Address: msg.GetAddress(),
			}
			if m := msg.GetErrorMessage(); m != "" {
				upd.Err = errors.New(m)
			}
			select {
			case <-ctx.Done():
				return
			case out <- upd:
			}
		}
	}()
	return out, nil
}

func (c *grpcClient) HealthCheck(ctx context.Context) error {
	_, err := c.client.HealthCheck(ctx, &fleetpb.HealthCheckRequest{})
	return errFromStatus(err)
}

// Ping is the host's plugin-liveness probe; not part of fleet.Backend.
func (c *grpcClient) Ping(ctx context.Context) error {
	_, err := c.client.Ping(ctx, &fleetpb.PingRequest{})
	return errFromStatus(err)
}
