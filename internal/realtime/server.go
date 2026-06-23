package realtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/ggscale/ggscale/internal/cache"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/enduser"
)

// Options configures ServeWS. Hub is required; everything else is optional.
type Options struct {
	Hub *Hub

	// Cache + MaxPerTenant enable a per-tenant concurrent-connection cap.
	// When MaxPerTenant is 0 the cap is disabled and Cache is unused.
	Cache        cache.Store
	MaxPerTenant int64

	// MaxPerEndUser caps simultaneous connections from a single end-user
	// (one player). Stops a single player from opening N sockets and
	// burning the entire per-tenant budget. 0 disables the per-user cap.
	MaxPerEndUser int64

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
// the connection into the Hub. The tenant + enduser middlewares must run
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
		endUserID, ok := enduser.IDFromContext(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var refreshSlots func(context.Context)

		// Per-end-user cap first: a single misbehaving player can't drain
		// the per-tenant budget for everyone else. The order matters —
		// failing the user-level cap before the tenant one means a
		// flapping user doesn't briefly hold a tenant slot before being
		// rejected.
		if opts.MaxPerEndUser > 0 && opts.Cache != nil {
			userKey := slotKeyForEndUser(tenantID, endUserID)
			acquired, _, slotErr := opts.Cache.AcquireSlot(r.Context(), userKey, opts.MaxPerEndUser, slotTTL)
			if slotErr != nil {
				logger.Error("realtime: AcquireSlot (end_user) failed", "err", slotErr, "tenant_id", tenantID, "end_user_id", endUserID)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !acquired {
				http.Error(w, "too many connections for this user", http.StatusServiceUnavailable)
				return
			}
			defer func() {
				if rerr := opts.Cache.ReleaseSlot(context.Background(), userKey); rerr != nil {
					logger.Warn("realtime: ReleaseSlot (end_user) failed", "err", rerr, "tenant_id", tenantID, "end_user_id", endUserID)
				}
			}()
			refreshSlots = appendRefresh(refreshSlots, func(ctx context.Context) {
				if rerr := opts.Cache.RefreshSlot(ctx, userKey, slotTTL); rerr != nil {
					logger.Warn("realtime: RefreshSlot (end_user) failed", "err", rerr, "tenant_id", tenantID, "end_user_id", endUserID)
				}
			})
		}

		slotKey := slotKeyForTenant(tenantID)
		if opts.MaxPerTenant > 0 && opts.Cache != nil {
			acquired, _, slotErr := opts.Cache.AcquireSlot(r.Context(), slotKey, opts.MaxPerTenant, slotTTL)
			if slotErr != nil {
				logger.Error("realtime: AcquireSlot failed", "err", slotErr, "tenant_id", tenantID)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !acquired {
				http.Error(w, "too many connections", http.StatusServiceUnavailable)
				return
			}
			defer func() {
				if rerr := opts.Cache.ReleaseSlot(context.Background(), slotKey); rerr != nil {
					logger.Warn("realtime: ReleaseSlot failed", "err", rerr, "tenant_id", tenantID)
				}
			}()
			refreshSlots = appendRefresh(refreshSlots, func(ctx context.Context) {
				if rerr := opts.Cache.RefreshSlot(ctx, slotKey, slotTTL); rerr != nil {
					logger.Warn("realtime: RefreshSlot failed", "err", rerr, "tenant_id", tenantID)
				}
			})
		}

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			logger.Warn("realtime: ws upgrade failed", "err", err)
			return
		}
		conn.SetReadLimit(maxReadSize)
		defer conn.CloseNow() //nolint:errcheck // best-effort cleanup

		writer := &wsWriter{ws: conn}
		unregister := opts.Hub.Register(tenantID, endUserID, writer)
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

type wsWriter struct {
	ws *websocket.Conn
}

func (w *wsWriter) Write(ctx context.Context, data []byte) error {
	return w.ws.Write(ctx, websocket.MessageText, data)
}

func (w *wsWriter) Close() error {
	return w.ws.Close(websocket.StatusNormalClosure, "")
}

func slotKeyForTenant(tenantID int64) string {
	return "realtime:tenant:" + strconv.FormatInt(tenantID, 10)
}

func slotKeyForEndUser(tenantID, endUserID int64) string {
	return "realtime:tenant:" + strconv.FormatInt(tenantID, 10) + ":user:" + strconv.FormatInt(endUserID, 10)
}
