package realtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ggscale/ggscale/internal/cache"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/tenant"
)

// Options configures ServeWS. Hub is required; everything else is optional.
type Options struct {
	Hub *Hub

	// Cache backs the per-player connection cap. Unused when MaxPerPlayer is 0.
	Cache cache.Store

	// TenantCap enforces the per-tenant CCU cap using the tier-aware burst
	// model (ConnectionCapForClass). nil disables the per-tenant cap. The
	// tenant's class is read from the request's API key; the limit is the
	// class envelope unless EnvMaxPerTenant overrides it.
	TenantCap ratelimit.ConnectionCap

	// EnvMaxPerTenant, when > 0, overrides the tier-derived per-tenant cap
	// with a fixed hard limit (no burst) — an operator escape hatch for
	// self-host. 0 means use the tenant's class envelope.
	EnvMaxPerTenant int64

	// MaxPerPlayer caps simultaneous connections from a single player
	// (one player). Stops a single player from opening N sockets and
	// burning the entire per-tenant budget. 0 disables the per-user cap.
	MaxPerPlayer int64

	// HeartbeatInterval controls server-initiated WebSocket pings. Zero
	// uses 30s in production; tests pass a large value to disable.
	HeartbeatInterval time.Duration

	// SlotTTL bounds how long a per-tenant slot survives without
	// refresh. Defaults to HeartbeatInterval*3 (so two missed refreshes
	// before reap).
	SlotTTL time.Duration

	Logger *slog.Logger
}

const (
	defaultHeartbeatInterval = 30 * time.Second
	maxReadSize              = 1 << 20 // 1 MiB per inbound message
)

// ServeWS returns the HTTP handler that upgrades to a WebSocket and ties
// the connection into the Hub. The tenant + player middlewares must run
// first so the request context already carries both ids.
func ServeWS(opts Options) http.HandlerFunc {
	heartbeat := opts.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = defaultHeartbeatInterval
	}
	slotTTL := opts.SlotTTL
	if slotTTL <= 0 {
		slotTTL = heartbeat * 3
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return func(w http.ResponseWriter, r *http.Request) {
		tenantID, err := db.TenantFromContext(r.Context())
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		playerID, ok := playerauth.IDFromContext(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var refreshSlots func(context.Context)

		// Per-player cap first: a single misbehaving player can't drain
		// the per-tenant budget for everyone else. The order matters —
		// failing the user-level cap before the tenant one means a
		// flapping user doesn't briefly hold a tenant slot before being
		// rejected.
		if opts.MaxPerPlayer > 0 && opts.Cache != nil {
			userKey := slotKeyForPlayer(tenantID, playerID)
			acquired, _, slotErr := opts.Cache.AcquireSlot(r.Context(), userKey, opts.MaxPerPlayer, slotTTL)
			if slotErr != nil {
				logger.Error("realtime: AcquireSlot (player) failed", "err", slotErr, "tenant_id", tenantID, "player_id", playerID)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !acquired {
				http.Error(w, "too many connections for this user", http.StatusServiceUnavailable)
				return
			}
			defer func() {
				if rerr := opts.Cache.ReleaseSlot(context.Background(), userKey); rerr != nil {
					logger.Warn("realtime: ReleaseSlot (player) failed", "err", rerr, "tenant_id", tenantID, "player_id", playerID)
				}
			}()
			refreshSlots = appendRefresh(refreshSlots, func(ctx context.Context) {
				if rerr := opts.Cache.RefreshSlot(ctx, userKey, slotTTL); rerr != nil {
					logger.Warn("realtime: RefreshSlot (player) failed", "err", rerr, "tenant_id", tenantID, "player_id", playerID)
				}
			})
		}

		// Per-tenant CCU cap: tier-aware burst envelope, enforced in the shared
		// cache so it is one global limit across app hosts (approximate on the
		// distributed backend — CCU caps are guardrails, not exact promises).
		if opts.TenantCap != nil {
			caps := tenantCapLimits(tenantTierFromContext(r.Context()), opts.EnvMaxPerTenant)
			capKey := ratelimit.ConnectionCapKey(tenantID)
			decision, capErr := opts.TenantCap.Acquire(r.Context(), capKey, caps)
			switch {
			case capErr != nil:
				// Fail open: a cache blip must not drop players. Log + carry on
				// without a tenant slot (nothing to release or refresh).
				logger.Error("realtime: tenant cap acquire failed; allowing connection (fail-open)",
					"err", capErr, "tenant_id", tenantID)
			case !decision.Allowed:
				w.Header().Set("Retry-After", strconv.Itoa(int(tenantCapRetryAfter.Seconds())))
				http.Error(w, "too many connections", http.StatusServiceUnavailable)
				return
			default:
				defer func() {
					if rerr := opts.TenantCap.Release(context.Background(), capKey); rerr != nil {
						logger.Warn("realtime: tenant cap release failed", "err", rerr, "tenant_id", tenantID)
					}
				}()
				refreshSlots = appendRefresh(refreshSlots, func(ctx context.Context) {
					if rerr := opts.TenantCap.Refresh(ctx, capKey); rerr != nil {
						logger.Warn("realtime: tenant cap refresh failed", "err", rerr, "tenant_id", tenantID)
					}
				})
			}
		}

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			logger.Warn("realtime: ws upgrade failed", "err", err)
			return
		}
		conn.SetReadLimit(maxReadSize)
		defer conn.CloseNow() //nolint:errcheck // best-effort cleanup

		writer := &wsWriter{ws: conn}
		unregister := opts.Hub.Register(tenantID, playerID, writer)
		defer unregister()

		runConnection(r.Context(), conn, heartbeat, refreshSlots, logger)
	}
}

func appendRefresh(current func(context.Context), next func(context.Context)) func(context.Context) {
	if current == nil {
		return next
	}
	return func(ctx context.Context) {
		current(ctx)
		next(ctx)
	}
}

// runConnection drives the per-client read loop alongside a heartbeat
// ticker. It returns when the client disconnects, the heartbeat ping
// fails, or ctx is cancelled.
func runConnection(ctx context.Context, conn *websocket.Conn, heartbeat time.Duration, refreshSlots func(context.Context), logger *slog.Logger) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	heartbeatErr := make(chan error, 1)
	go func() { heartbeatErr <- runHeartbeat(ctx, conn, heartbeat, refreshSlots) }()

	for {
		_, _, err := conn.Read(ctx)
		if err != nil {
			if !isExpectedCloseErr(err) {
				logger.Debug("realtime: ws read ended", "err", err)
			}
			cancel()
			<-heartbeatErr
			return
		}
		if refreshSlots != nil {
			refreshSlots(context.Background())
		}
		// Inbound messages are currently ignored; lobby + chat handlers
		// are deferred.
	}
}

func runHeartbeat(ctx context.Context, conn *websocket.Conn, interval time.Duration, refreshSlots func(context.Context)) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			pingCtx, cancel := context.WithTimeout(ctx, interval)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return err
			}
			if refreshSlots != nil {
				refreshSlots(context.Background())
			}
		}
	}
}

func isExpectedCloseErr(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return true
	}
	closeStatus := websocket.CloseStatus(err)
	return closeStatus == websocket.StatusNormalClosure || closeStatus == websocket.StatusGoingAway
}

// writeTimeout bounds a single frame write so a stuck or slow-draining socket
// (full send buffer, half-dead peer) can't block the sender — the matchmaker or
// invite goroutine — indefinitely. The write fails fast and the read loop reaps
// the connection.
const writeTimeout = 10 * time.Second

// wsWriter adapts a *websocket.Conn to the Hub's Writer seam. coder/websocket
// forbids concurrent writes to one connection, and a single player can be
// targeted by several subsystems at once (matchmaker, invites, presence), so
// mu serializes frames and Close on this socket.
type wsWriter struct {
	mu sync.Mutex
	ws *websocket.Conn
}

func (w *wsWriter) Write(ctx context.Context, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ws.Write(ctx, websocket.MessageText, data)
}

func (w *wsWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ws.Close(websocket.StatusNormalClosure, "")
}

func slotKeyForPlayer(tenantID, playerID int64) string {
	return "realtime:tenant:" + strconv.FormatInt(tenantID, 10) + ":user:" + strconv.FormatInt(playerID, 10)
}

// tenantCapRetryAfter is advertised to a rejected client so its SDK backs off
// before retrying, per the "try again later" semantics of the CCU cap.
const tenantCapRetryAfter = 5 * time.Second

// tenantTierFromContext reads the tenant's service class from the request's API
// key. Behind the tenant middleware the key is always present; the Tier0
// fallback (smallest envelope) is a fail-safe for the can't-happen case.
func tenantTierFromContext(ctx context.Context) tenant.Tier {
	key, ok := tenant.APIKeyFromContext(ctx)
	if !ok {
		return tenant.Tier0
	}
	return key.Tier
}

// tenantCapLimits resolves the per-tenant connection envelope: the tenant's
// class limits by default, or a fixed hard cap (no burst) when an operator
// pins EnvMaxPerTenant.
func tenantCapLimits(tier tenant.Tier, envOverride int64) ratelimit.CapLimits {
	if envOverride > 0 {
		return ratelimit.CapLimits{Sustained: envOverride, Ceiling: envOverride}
	}
	return ratelimit.ConnectionCapForClass(tier)
}
