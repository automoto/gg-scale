// Package agones implements a fleet.Backend backed by an Agones-controlled
// Kubernetes cluster. Game servers are managed as Agones GameServer CRDs;
// allocation goes through the GameServerAllocation CR (single-cluster
// path — multi-cluster allocator-service support is a follow-on).
package agones

import (
	"context"
	"errors"
	"fmt"
	"strings"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	agonesclientset "agones.dev/agones/pkg/client/clientset/versioned"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ggscale/ggscale/internal/fleet"
)

// API narrows the Agones clientset surface to the operations this backend
// actually performs. Real callers pass an *agonesclientset.Clientset
// wrapped by clientsetAdapter; tests inject a fake.
type API interface {
	CreateGameServerAllocation(ctx context.Context, namespace string, gsa *allocationv1.GameServerAllocation) (*allocationv1.GameServerAllocation, error)
	DeleteGameServer(ctx context.Context, namespace, name string, opts metav1.DeleteOptions) error
	GetGameServer(ctx context.Context, namespace, name string) (*agonesv1.GameServer, error)
	WatchGameServer(ctx context.Context, namespace, name string) (<-chan GameServerEvent, error)
	Ping(ctx context.Context) error
}

// GameServerEvent is the per-resource update the watch implementation
// surfaces. Err is non-nil only on terminal watch failure.
type GameServerEvent struct {
	State agonesv1.GameServerState
	Err   error
}

// Config carries the host-wide agones knobs. Per-template values (Agones
// Fleet name and selector labels) come in through AllocationRequest.Config
// on each Allocate. The API field is set by NewFromConfig (production) or
// by tests directly.
type Config struct {
	API       API
	Namespace string
}

// Backend allocates Agones GameServers.
type Backend struct {
	cfg Config
}

// New builds a Backend, validating that the operator supplied enough
// host-level config (API client + default namespace).
func New(cfg Config) (*Backend, error) {
	if cfg.API == nil {
		return nil, errors.New("agones: API is required")
	}
	if cfg.Namespace == "" {
		return nil, errors.New("agones: Namespace is required")
	}
	return &Backend{cfg: cfg}, nil
}

// NewFromKubeconfig is the production entry point. An empty kubeconfigPath
// loads in-cluster config (server pods get a ServiceAccount mount); any
// other value is treated as a path on disk.
func NewFromKubeconfig(cfg Config, kubeconfigPath string) (*Backend, error) {
	rcfg, err := loadRESTConfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}
	cs, err := agonesclientset.NewForConfig(rcfg)
	if err != nil {
		return nil, fmt.Errorf("agones: clientset: %w", err)
	}
	cfg.API = clientsetAdapter{cs}
	return New(cfg)
}

// Name is the identifier persisted on every allocation row.
func (b *Backend) Name() string { return "agones" }

// Template carries the per-allocation knobs the agones backend reads off
// AllocationRequest.Config: the Agones Fleet to allocate from, optional
// namespace override, and any additional selector labels.
type Template struct {
	Namespace      string
	FleetName      string
	SelectorLabels map[string]string
}

// TemplateFromConfig parses AllocationRequest.Config keys into a Template
// and merges any "selector.<k>=v" entries into SelectorLabels.
func TemplateFromConfig(cfg map[string]string) Template {
	t := Template{
		Namespace:      cfg["namespace"],
		FleetName:      cfg["fleet_name"],
		SelectorLabels: map[string]string{},
	}
	for k, v := range cfg {
		if rest, ok := strings.CutPrefix(k, "selector."); ok && rest != "" {
			t.SelectorLabels[rest] = v
		}
	}
	if t.FleetName != "" {
		t.SelectorLabels["agones.dev/fleet"] = t.FleetName
	}
	return t
}

// Allocate creates a GameServerAllocation CR and reads the synchronous
// Status the Agones controller writes back. UnAllocated / Contention are
// surfaced as errors so the manager can retry or fail.
func (b *Backend) Allocate(ctx context.Context, req fleet.AllocationRequest) (*fleet.Allocation, error) {
	tmpl := TemplateFromConfig(req.Config)
	namespace := b.resolveNamespace(tmpl.Namespace)
	gsa := &allocationv1.GameServerAllocation{
		Spec: allocationv1.GameServerAllocationSpec{
			Selectors: []allocationv1.GameServerSelector{
				{
					LabelSelector: metav1.LabelSelector{MatchLabels: selectorWithRegion(tmpl.SelectorLabels, req.Region)},
				},
			},
		},
	}
	result, err := b.cfg.API.CreateGameServerAllocation(ctx, namespace, gsa)
	if err != nil {
		return nil, fmt.Errorf("agones: allocate: %w", err)
	}
	if result.Status.State != allocationv1.GameServerAllocationAllocated {
		return nil, fmt.Errorf("agones: allocate state=%s gs=%q", result.Status.State, result.Status.GameServerName)
	}
	port := int32(0)
	if len(result.Status.Ports) > 0 {
		port = result.Status.Ports[0].Port
	}
	if result.Status.Address == "" || port == 0 {
		return nil, fmt.Errorf("agones: allocated gs %q has no address/port", result.Status.GameServerName)
	}
	// The allocation Status doesn't carry the port protocol. Read it
	// off the GameServer Spec so the matchmaker response can include a
	// protocol_hint. A GetGameServer call hits the informer cache in-
	// cluster and adds a few milliseconds; on failure we log via the
	// returned error and leave Protocol empty rather than fail the
	// allocation — protocol_hint is observability, not a hard contract.
	protocol := ""
	if gs, getErr := b.cfg.API.GetGameServer(ctx, namespace, result.Status.GameServerName); getErr == nil && gs != nil && gs.Spec.Ports != nil && len(gs.Spec.Ports) > 0 {
		protocol = strings.ToLower(string(gs.Spec.Ports[0].Protocol))
	}
	return &fleet.Allocation{
		BackendRef: namespace + "/" + result.Status.GameServerName,
		Address:    fmt.Sprintf("%s:%d", result.Status.Address, port),
		Protocol:   protocol,
		Status:     fleet.StatusReady,
	}, nil
}

// splitBackendRef parses "<namespace>/<gsName>" produced by Allocate. Older
// allocations from before this format are routed to b.cfg.Namespace as a
// fallback (the format change is backwards-compatible at read time).
func (b *Backend) splitBackendRef(ref string) (ns, name string) {
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return b.cfg.Namespace, ref
}

func (b *Backend) resolveNamespace(tmplNamespace string) string {
	if tmplNamespace != "" {
		return tmplNamespace
	}
	return b.cfg.Namespace
}

// Deallocate deletes the GameServer CR; Agones reaps the underlying pod.
// A "not found" delete is treated as success (idempotent shutdown).
func (b *Backend) Deallocate(ctx context.Context, _ fleet.AllocationID, backendRef string) error {
	namespace, name := b.splitBackendRef(backendRef)
	if err := b.cfg.API.DeleteGameServer(ctx, namespace, name, metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("agones: delete gs %q: %w", name, err)
	}
	return nil
}

// Status maps the current GameServer CR state into the fleet lifecycle.
func (b *Backend) Status(ctx context.Context, _ fleet.AllocationID, backendRef string) (fleet.Status, error) {
	namespace, name := b.splitBackendRef(backendRef)
	gs, err := b.cfg.API.GetGameServer(ctx, namespace, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fleet.StatusShutdown, nil
		}
		return fleet.StatusFailed, fmt.Errorf("agones: get gs %q: %w", name, err)
	}
	return translateState(gs.Status.State), nil
}

// Watch consumes the API-supplied event stream and surfaces fleet
// StatusUpdates. The channel closes when the source closes or ctx is
// cancelled.
func (b *Backend) Watch(ctx context.Context, _ fleet.AllocationID, backendRef string) (<-chan fleet.StatusUpdate, error) {
	namespace, name := b.splitBackendRef(backendRef)
	src, err := b.cfg.API.WatchGameServer(ctx, namespace, name)
	if err != nil {
		return nil, fmt.Errorf("agones: watch gs %q: %w", name, err)
	}
	out := make(chan fleet.StatusUpdate, 4)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-src:
				if !ok {
					return
				}
				if ev.Err != nil {
					select {
					case out <- fleet.StatusUpdate{Status: fleet.StatusFailed, Err: ev.Err}:
					case <-ctx.Done():
					}
					return
				}
				select {
				case out <- fleet.StatusUpdate{Status: translateState(ev.State)}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// HealthCheck verifies the agones API server is reachable.
func (b *Backend) HealthCheck(ctx context.Context) error { return b.cfg.API.Ping(ctx) }

func selectorWithRegion(labels map[string]string, region string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	if region != "" {
		out["ggscale.region"] = region
	}
	return out
}

func translateState(s agonesv1.GameServerState) fleet.Status {
	switch s {
	case agonesv1.GameServerStateReady:
		return fleet.StatusReady
	case agonesv1.GameServerStateAllocated:
		return fleet.StatusAllocated
	case agonesv1.GameServerStateShutdown:
		return fleet.StatusShutdown
	case agonesv1.GameServerStateError, agonesv1.GameServerStateUnhealthy:
		return fleet.StatusFailed
	default:
		return fleet.StatusAllocating
	}
}

func loadRESTConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath == "" {
		c, err := rest.InClusterConfig()
		if err == nil {
			return c, nil
		}
		// Fall through — operator may run ggscale outside the cluster.
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		rules.ExplicitPath = kubeconfigPath
	}
	cc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("agones: kubeconfig: %w", err)
	}
	return cc, nil
}

// clientsetAdapter narrows the production Agones clientset to the API
// interface. WatchGameServer translates the k8s watch.Interface event
// stream into our own GameServerEvent type so the Backend doesn't depend
// on apimachinery types.
type clientsetAdapter struct {
	cs *agonesclientset.Clientset
}

func (a clientsetAdapter) CreateGameServerAllocation(ctx context.Context, ns string, gsa *allocationv1.GameServerAllocation) (*allocationv1.GameServerAllocation, error) {
	return a.cs.AllocationV1().GameServerAllocations(ns).Create(ctx, gsa, metav1.CreateOptions{})
}

func (a clientsetAdapter) DeleteGameServer(ctx context.Context, ns, name string, opts metav1.DeleteOptions) error {
	return a.cs.AgonesV1().GameServers(ns).Delete(ctx, name, opts)
}

func (a clientsetAdapter) GetGameServer(ctx context.Context, ns, name string) (*agonesv1.GameServer, error) {
	return a.cs.AgonesV1().GameServers(ns).Get(ctx, name, metav1.GetOptions{})
}

func (a clientsetAdapter) WatchGameServer(ctx context.Context, ns, name string) (<-chan GameServerEvent, error) {
	// fields.OneTermEqualSelector escapes the value safely. Kubernetes
	// object names follow DNS-1123 so injection is implausible today,
	// but the typed selector costs nothing and is the idiomatic API.
	sel := fields.OneTermEqualSelector("metadata.name", name).String()
	w, err := a.cs.AgonesV1().GameServers(ns).Watch(ctx, metav1.ListOptions{FieldSelector: sel})
	if err != nil {
		return nil, err
	}
	out := make(chan GameServerEvent, 4)
	go func() {
		defer close(out)
		defer w.Stop()
		// send respects ctx so a slow / cancelled consumer doesn't
		// pin this goroutine forever on a full channel buffer.
		send := func(ev GameServerEvent) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.ResultChan():
				if !ok {
					return
				}
				gs, gsOK := ev.Object.(*agonesv1.GameServer)
				if !gsOK {
					if ev.Type == watch.Error {
						_ = send(GameServerEvent{Err: fmt.Errorf("agones: watch error: %v", ev.Object)})
						return
					}
					continue
				}
				if !send(GameServerEvent{State: gs.Status.State}) {
					return
				}
			}
		}
	}()
	return out, nil
}

func (a clientsetAdapter) Ping(ctx context.Context) error {
	_, err := a.cs.Discovery().ServerVersion()
	if err != nil {
		return err
	}
	_ = ctx
	return nil
}
