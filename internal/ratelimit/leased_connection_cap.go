package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	defaultGrantLease              = 45 * time.Second
	defaultRenewInterval           = 15 * time.Second
	defaultGrantOpTimeout          = 500 * time.Millisecond
	defaultGrantRenewTimeout       = 5 * time.Second
	minimumGrantBlock        int64 = 32
	maximumGrantBlock        int64 = 512
)

type grantRequest struct {
	TenantID  int64
	Region    string
	HolderID  string
	Used      int64
	Requested int64
	Caps      CapLimits
	Lease     time.Duration
}

type grantRelease struct {
	TenantID int64
	Region   string
	HolderID string
}

type grantRenewal struct {
	TenantID int64
	Used     int64
}

type grantRenewRequest struct {
	Region   string
	HolderID string
	Lease    time.Duration
	Grants   []grantRenewal
}

type grantResult struct {
	Allocated int64
	Current   int64
	Reason    string
}

type grantStore interface {
	Sync(ctx context.Context, req grantRequest) (grantResult, error)
	Renew(ctx context.Context, req grantRenewRequest) (map[int64]struct{}, error)
	Release(ctx context.Context, req grantRelease) error
}

type leasedCapOptions struct {
	Region           string
	HolderID         string
	Lease            time.Duration
	RenewInterval    time.Duration
	OperationTimeout time.Duration
	Now              func() time.Time
}

type localGrant struct {
	mu sync.Mutex

	refs            int
	caps            CapLimits
	allocated       int64
	used            int64
	emergency       int64
	reportedUsed    int64
	regionalCurrent int64
	expiresAt       time.Time
}

// LeasedConnectionCap keeps connection admissions in process memory and only
// contacts the shared grant store when a tenant needs capacity or a live grant
// must be renewed. One instance is owned by one application process.
type LeasedConnectionCap struct {
	store            grantStore
	region           string
	holderID         string
	lease            time.Duration
	renewInterval    time.Duration
	operationTimeout time.Duration
	now              func() time.Time

	mu     sync.Mutex
	grants map[int64]*localGrant

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	rejections          *prometheus.CounterVec
	emergencyAdmissions prometheus.Counter
	storeSyncs          *prometheus.CounterVec
}

func newLeasedConnectionCap(store grantStore, reg prometheus.Registerer, opts leasedCapOptions) *LeasedConnectionCap {
	if opts.Region == "" {
		opts.Region = "local"
	}
	if opts.HolderID == "" {
		opts.HolderID = uuid.NewString()
	}
	if opts.Lease <= 0 {
		opts.Lease = defaultGrantLease
	}
	if opts.RenewInterval == 0 {
		opts.RenewInterval = defaultRenewInterval
	}
	if opts.OperationTimeout <= 0 {
		opts.OperationTimeout = defaultGrantOpTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	c := &LeasedConnectionCap{
		store:            store,
		region:           opts.Region,
		holderID:         opts.HolderID,
		lease:            opts.Lease,
		renewInterval:    opts.RenewInterval,
		operationTimeout: opts.OperationTimeout,
		now:              opts.Now,
		grants:           make(map[int64]*localGrant),
		stop:             make(chan struct{}),
	}
	c.registerMetrics(reg)
	if c.renewInterval > 0 {
		c.wg.Add(1)
		go c.renewLoop()
	}
	return c
}

func (c *LeasedConnectionCap) registerMetrics(reg prometheus.Registerer) {
	if reg == nil {
		return
	}
	c.rejections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ggscale_connection_cap_rejections_total",
		Help: "WebSocket connections rejected by the tenant CCU cap, by reason.",
	}, []string{"reason"})
	c.emergencyAdmissions = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ggscale_connection_cap_emergency_admissions_total",
		Help: "WebSocket admissions made from the bounded local emergency allowance.",
	})
	c.storeSyncs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ggscale_connection_cap_grant_sync_total",
		Help: "PostgreSQL connection-cap grant synchronization attempts by result.",
	}, []string{"result"})
	reg.MustRegister(c.rejections, c.emergencyAdmissions, c.storeSyncs)
}

// Acquire reserves one process-local permit for tenantID. A valid local grant
// is the fast path. Grant synchronization failures use a small bounded
// emergency allowance; exhaustion rejects instead of failing open without
// limit.
func (c *LeasedConnectionCap) Acquire(ctx context.Context, tenantID int64, caps CapLimits) (CapDecision, error) {
	grant := c.retainGrant(tenantID)
	grant.mu.Lock()
	defer func() {
		c.releaseGrantRef(tenantID, grant)
		grant.mu.Unlock()
	}()

	if sameCaps(grant.caps, caps) && grant.used < grant.allocated && c.now().Before(grant.expiresAt) {
		grant.used++
		return CapDecision{Allowed: true, Current: grant.current()}, nil
	}

	reportedUsed := grant.used
	result, err := c.sync(ctx, grantRequest{
		TenantID:  tenantID,
		Region:    c.region,
		HolderID:  c.holderID,
		Used:      reportedUsed,
		Requested: grantBlockSize(caps.Ceiling),
		Caps:      caps,
		Lease:     c.lease,
	})
	if err != nil {
		return c.acquireEmergency(grant, caps), nil
	}

	grant.caps = caps
	grant.allocated = result.Allocated
	grant.reportedUsed = reportedUsed
	grant.regionalCurrent = result.Current
	grant.expiresAt = c.now().Add(c.localGrantValidity())
	if grant.used >= grant.allocated {
		reason := result.Reason
		if reason == "" {
			reason = CapRejectCeiling
		}
		c.reject(reason)
		return CapDecision{Current: grant.current(), Reason: reason}, nil
	}
	grant.used++
	return CapDecision{Allowed: true, Current: grant.current()}, nil
}

func (c *LeasedConnectionCap) acquireEmergency(grant *localGrant, caps CapLimits) CapDecision {
	limit := emergencyAllowance(caps.Sustained)
	if grant.emergency >= limit {
		c.reject(CapRejectUnavailable)
		return CapDecision{Current: grant.current(), Reason: CapRejectUnavailable}
	}
	grant.emergency++
	if c.emergencyAdmissions != nil {
		c.emergencyAdmissions.Inc()
	}
	return CapDecision{Allowed: true, Current: grant.current(), Emergency: true}
}

// Release returns one local permit. Emergency permits are returned first; this
// keeps total admissions bounded even though the caller does not need to carry
// a backend-specific reservation token.
func (c *LeasedConnectionCap) Release(ctx context.Context, tenantID int64) error {
	grant := c.retainGrant(tenantID)
	grant.mu.Lock()
	defer func() {
		c.releaseGrantRef(tenantID, grant)
		grant.mu.Unlock()
	}()

	switch {
	case grant.emergency > 0:
		grant.emergency--
	case grant.used > 0:
		grant.used--
	default:
		return nil
	}
	if grant.used+grant.emergency > 0 {
		if c.shouldReclaim(grant) {
			return c.reclaim(ctx, tenantID, grant)
		}
		return nil
	}

	// Keep this lock across the database delete. Otherwise an acquire for the
	// same holder could recreate the row and then have this release delete it.
	grant.allocated = 0
	grant.reportedUsed = 0
	grant.regionalCurrent = 0
	grant.expiresAt = time.Time{}
	return c.releaseStore(ctx, tenantID)
}

func (c *LeasedConnectionCap) shouldReclaim(grant *localGrant) bool {
	if grant.used == 0 || grant.emergency > 0 || grant.caps.Ceiling <= 0 {
		return false
	}
	block := grantBlockSize(grant.caps.Ceiling)
	return grant.allocated-grant.used > 2*block
}

func (c *LeasedConnectionCap) reclaim(ctx context.Context, tenantID int64, grant *localGrant) error {
	result, err := c.sync(ctx, grantRequest{
		TenantID:  tenantID,
		Region:    c.region,
		HolderID:  c.holderID,
		Used:      grant.used,
		Requested: grantBlockSize(grant.caps.Ceiling),
		Caps:      grant.caps,
		Lease:     c.lease,
	})
	if err != nil {
		return err
	}
	grant.allocated = result.Allocated
	grant.reportedUsed = grant.used
	grant.regionalCurrent = result.Current
	grant.expiresAt = c.now().Add(c.localGrantValidity())
	return nil
}

func (c *LeasedConnectionCap) retainGrant(tenantID int64) *localGrant {
	c.mu.Lock()
	defer c.mu.Unlock()
	grant := c.grants[tenantID]
	if grant == nil {
		grant = &localGrant{}
		c.grants[tenantID] = grant
	}
	grant.refs++
	return grant
}

// releaseGrantRef is called with grant.mu held. refs is protected by c.mu;
// the grant fields in the reap condition are protected by grant.mu.
func (c *LeasedConnectionCap) releaseGrantRef(tenantID int64, grant *localGrant) {
	c.mu.Lock()
	defer c.mu.Unlock()
	grant.refs--
	if grant.refs > 0 || grant.used > 0 || grant.emergency > 0 || grant.allocated > 0 {
		return
	}
	if c.grants[tenantID] == grant {
		delete(c.grants, tenantID)
	}
}

func (c *LeasedConnectionCap) sync(ctx context.Context, req grantRequest) (grantResult, error) {
	opCtx, cancel := c.operationContext(ctx)
	defer cancel()
	result, err := c.store.Sync(opCtx, req)
	if c.storeSyncs != nil {
		label := "ok"
		if err != nil {
			label = "error"
		}
		c.storeSyncs.WithLabelValues(label).Inc()
	}
	if err != nil {
		return grantResult{}, fmt.Errorf("connection cap grant sync: %w", err)
	}
	return result, nil
}

func (c *LeasedConnectionCap) releaseStore(ctx context.Context, tenantID int64) error {
	opCtx, cancel := c.operationContext(ctx)
	defer cancel()
	if err := c.store.Release(opCtx, grantRelease{
		TenantID: tenantID,
		Region:   c.region,
		HolderID: c.holderID,
	}); err != nil {
		return fmt.Errorf("connection cap grant release: %w", err)
	}
	return nil
}

func (c *LeasedConnectionCap) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, c.operationTimeout)
}

func (c *LeasedConnectionCap) renewLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.renewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.renewAll()
		}
	}
}

func (c *LeasedConnectionCap) renewAll() {
	c.mu.Lock()
	grants := make(map[int64]*localGrant, len(c.grants))
	for tenantID, grant := range c.grants {
		grant.refs++
		grants[tenantID] = grant
	}
	c.mu.Unlock()

	renewals := make([]grantRenewal, 0, len(grants))
	for tenantID, grant := range grants {
		grant.mu.Lock()
		if grant.used > 0 {
			renewals = append(renewals, grantRenewal{TenantID: tenantID, Used: grant.used})
		} else {
			c.releaseGrantRef(tenantID, grant)
			delete(grants, tenantID)
		}
		grant.mu.Unlock()
	}
	if len(renewals) == 0 {
		return
	}
	opCtx, cancel := context.WithTimeout(context.Background(), defaultGrantRenewTimeout)
	renewed, err := c.store.Renew(opCtx, grantRenewRequest{
		Region:   c.region,
		HolderID: c.holderID,
		Lease:    c.lease,
		Grants:   renewals,
	})
	cancel()
	if c.storeSyncs != nil {
		label := "ok"
		switch {
		case err != nil:
			label = "error"
		case len(renewed) != len(renewals):
			label = "partial"
		}
		c.storeSyncs.WithLabelValues(label).Inc()
	}
	localExpiresAt := c.now().Add(c.localGrantValidity())
	for _, renewal := range renewals {
		grant := grants[renewal.TenantID]
		grant.mu.Lock()
		_, ok := renewed[renewal.TenantID]
		if err == nil && ok && grant.used > 0 {
			grant.reportedUsed = renewal.Used
			grant.expiresAt = localExpiresAt
		}
		c.releaseGrantRef(renewal.TenantID, grant)
		grant.mu.Unlock()
	}
}

// Close stops renewal and releases this process's grants. Failed releases are
// returned together; their short database leases remain the recovery bound.
func (c *LeasedConnectionCap) Close(ctx context.Context) error {
	c.stopOnce.Do(func() { close(c.stop) })
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}

	c.mu.Lock()
	grants := make(map[int64]*localGrant, len(c.grants))
	for tenantID, grant := range c.grants {
		grants[tenantID] = grant
	}
	c.mu.Unlock()

	var errs []error
	for tenantID, grant := range grants {
		grant.mu.Lock()
		active := grant.allocated > 0 || grant.used > 0
		grant.mu.Unlock()
		if !active {
			continue
		}
		if err := c.releaseStore(ctx, tenantID); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("connection cap close: %w", err)
	}
	return nil
}

func (g *localGrant) current() int64 {
	current := g.regionalCurrent + (g.used - g.reportedUsed) + g.emergency
	return max(0, current)
}

func grantBlockSize(ceiling int64) int64 {
	return min(maximumGrantBlock, max(minimumGrantBlock, ceiling/200))
}

func emergencyAllowance(sustained int64) int64 {
	return min(64, max(8, sustained/1000))
}

func sameCaps(a, b CapLimits) bool {
	return a.Sustained == b.Sustained && a.Ceiling == b.Ceiling
}

func (c *LeasedConnectionCap) reject(reason string) {
	if c.rejections != nil {
		c.rejections.WithLabelValues(reason).Inc()
	}
}

// localGrantValidity stops local admission before the database lease can
// expire. It is duration-based, so host/database wall-clock skew cannot make a
// reclaimed grant appear valid in the process.
func (c *LeasedConnectionCap) localGrantValidity() time.Duration {
	return c.lease * 2 / 3
}
