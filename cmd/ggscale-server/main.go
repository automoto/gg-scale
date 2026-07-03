// Command ggscale-server is the ggscale control-plane HTTP server.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/ggscale/ggscale/internal/auth"
	cachebuild "github.com/ggscale/ggscale/internal/cache/build"
	"github.com/ggscale/ggscale/internal/config"
	"github.com/ggscale/ggscale/internal/dashboard"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	fleetbuild "github.com/ggscale/ggscale/internal/fleet/build"
	fleetplugin "github.com/ggscale/ggscale/internal/fleet/plugin"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/jobs"
	"github.com/ggscale/ggscale/internal/mailer"
	_ "github.com/ggscale/ggscale/internal/mailer/noop"
	_ "github.com/ggscale/ggscale/internal/mailer/smtp"
	"github.com/ggscale/ggscale/internal/matchmaker"
	"github.com/ggscale/ggscale/internal/middleware"
	migraterunner "github.com/ggscale/ggscale/internal/migrate"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/players"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/realtime"
	"github.com/ggscale/ggscale/internal/relay"
	"github.com/ggscale/ggscale/internal/serverlist"
	"github.com/ggscale/ggscale/internal/tenant"
)

// commit is overridden at build time via -ldflags.
var commit = "unknown"

func main() {
	if err := run(); err != nil {
		slog.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector())
	registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	// Apply forward-only SQL migrations before anything else touches the DB.
	// Runner returns ErrNoChange internally as a no-op so this is safe on
	// every restart.
	mr, err := migraterunner.New(cfg.DatabaseURL, cfg.MigrationsDir)
	if err != nil {
		return fmt.Errorf("migrate init: %w", err)
	}
	if err := mr.Up(); err != nil {
		_ = mr.Close()
		return fmt.Errorf("migrate up: %w", err)
	}
	_ = mr.Close()
	logger.Info("migrations applied", "dir", cfg.MigrationsDir)

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	poolCfg.ConnConfig.Tracer = observability.NewPgxTracer(registry)
	poolCfg.MaxConns = int32(cfg.DBMaxConns) //nolint:gosec // operator config, validated >= 4 by config.Validate
	poolCfg.MinConns = int32(cfg.DBMinConns) //nolint:gosec // operator config, validated >= 0
	poolCfg.MaxConnLifetime = cfg.DBMaxConnLifetime
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, "SET ROLE ggscale_app"); err != nil {
			return fmt.Errorf("set app db role: %w", err)
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := assertAppDBRole(ctx, pool); err != nil {
		return err
	}

	store, err := cachebuild.New(ctx, cachebuild.Config{
		Backend:             cfg.CacheBackend,
		OlricBindAddr:       cfg.CacheOlricBindAddr,
		OlricBindPort:       cfg.CacheOlricBindPort,
		OlricMemberlistAddr: cfg.CacheOlricMemberlistAddr,
		OlricMemberlistPort: cfg.CacheOlricMemberlistPort,
		OlricPeers:          cfg.CacheOlricPeers,
		Registry:            registry,
	})
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = store.Close(shutdownCtx)
	}()

	signer, err := auth.NewSignerFromHex(cfg.JWTSigningKey)
	if err != nil {
		return err
	}
	if cfg.JWTSigningKey == "" {
		slog.Warn("JWT_SIGNING_KEY not set; using a random in-process key — sessions won't survive restart")
	}

	m, err := mailer.New(cfg.MailProvider, cfg.SMTPAddr, cfg.SMTPUser, cfg.SMTPPassword, cfg.MailFrom, cfg.SMTPTLS)
	if err != nil {
		return fmt.Errorf("mailer: %w", err)
	}

	appPool := db.NewPoolWithTimeout(pool, cfg.DBStatementTimeout)
	authorizer, err := rbac.NewAuthorizer(appPool)
	if err != nil {
		return fmt.Errorf("rbac: %w", err)
	}
	defer authorizer.Close()
	var dashboardBootstrap *dashboard.Bootstrap
	if cfg.DashboardEnabled {
		dashboardBootstrap, err = dashboard.LoadBootstrap(ctx, appPool, cfg.DashboardBootstrapTokenFile, logger)
		if err != nil {
			return err
		}
	}

	fleetMgr, fleetCloser, err := buildFleet(cfg, appPool, logger)
	if err != nil {
		return err
	}
	if fleetCloser != nil {
		defer func() { _ = fleetCloser.Close() }()
	}
	pluginInfo := pluginInfoFromCloser(ctx, fleetMgr, fleetCloser)

	hub := realtime.NewHub()

	var relayIssuer *relay.Issuer
	if !cfg.FeatureP2PRelayEnabled {
		logger.Warn("p2p relay disabled: FEATURE_P2P_RELAY_ENABLED=false")
	}
	if cfg.FeatureP2PRelayEnabled && cfg.RelaySharedSecret != "" {
		relayIssuer = relay.NewIssuer(cfg.RelaySharedSecret, cfg.RelayRealm, cfg.RelayCredTTL)
		if cfg.RelayPublicIP != "" {
			relayServer, rerr := relay.NewServer(relay.ServerConfig{
				PublicIP: cfg.RelayPublicIP,
				BindAddr: cfg.RelayBindAddr,
				BindPort: cfg.RelayUDPPort,
				Issuer:   relayIssuer,
			})
			if rerr != nil {
				return fmt.Errorf("relay: %w", rerr)
			}
			defer func() { _ = relayServer.Close() }()
			logger.Info("relay server listening", "addr", cfg.RelayBindAddr, "port", cfg.RelayUDPPort, "public_ip", cfg.RelayPublicIP)
		} else {
			logger.Warn("relay credentials issued but no UDP listener: RELAY_PUBLIC_IP unset")
		}
	}

	// Server-browser heartbeat registry. TTL=15s tolerates a missed
	// heartbeat or two (game-servers send every 5s) without dropping
	// live entries; GC every 10s reclaims stale rows.
	serverListRegistry := serverlist.New(15 * time.Second)
	go serverListRegistry.RunGC(ctx, 10*time.Second)

	// Background jobs (River). Expired game-session/invite cleanup runs as a
	// leader-elected periodic job, so it fires once across the fleet rather
	// than once per instance. River failures are non-fatal — GC is best-effort
	// and the server must boot without it.
	if stopRiver := startRiverJobs(ctx, pool, appPool, logger); stopRiver != nil {
		defer stopRiver()
	}

	mmQueue := matchmaker.NewPGQueue(appPool)
	workerDone := make(chan struct{})
	workerCtx, cancelWorker := context.WithCancel(ctx)
	defer cancelWorker()
	if fleetMgr != nil {
		worker := matchmaker.NewWorker(mmQueue, fleetMgr, hub, matchmaker.WorkerConfig{
			BucketSize:    cfg.MatchmakerBucketSize,
			Interval:      cfg.MatchmakerInterval,
			ClaimTTL:      cfg.MatchmakerClaimTTL,
			MaxAttempts:   cfg.MatchmakerMaxAttempts,
			WorkerCount:   cfg.MatchmakerWorkerCount,
			SweepInterval: cfg.MatchmakerSweepInterval,
			Logger:        logger,
		})
		go func() {
			defer close(workerDone)
			worker.Run(workerCtx)
		}()
	} else {
		close(workerDone)
		logger.Warn("matchmaker worker disabled: no fleet backend configured")
	}

	router := httpapi.NewRouter(httpapi.Deps{
		Version:              "v1",
		Commit:               commit,
		Pool:                 appPool,
		Lookup:               tenant.NewSQLLookup(pool),
		Limiter:              ratelimit.NewCacheLimiter(store),
		Signer:               signer,
		Mailer:               m,
		MailFrom:             cfg.MailFrom,
		Cache:                store,
		Registry:             registry,
		RBAC:                 authorizer,
		Fleet:                fleetMgr,
		Hub:                  hub,
		RealtimeMaxPerTenant: cfg.RealtimeMaxPerTenant,
		RealtimeMaxPerPlayer: cfg.RealtimeMaxPerPlayer,
		Matchmaker:           mmQueue,
		ServerList:           serverListRegistry,
		RelayIssuer:          relayIssuer,
		Dashboard: dashboard.Config{
			Mount:              cfg.DashboardEnabled,
			CookieSecure:       cfg.DashboardCookieSecure,
			BaseURL:            cfg.DashboardBaseURL,
			MailFrom:           cfg.MailFrom,
			TrustedProxyHeader: cfg.TrustedProxyHeader,
			TrustedProxyCIDRs:  cfg.TrustedProxyCIDRs,
			FleetEnabled:       cfg.FeatureFleetEnabled,
			RelayEnabled:       cfg.FeatureP2PRelayEnabled,
		},
		Players: players.Config{
			Mount:        cfg.PlayersEnabled,
			CookieSecure: cfg.DashboardCookieSecure,
		},
		DashboardBootstrap:  dashboardBootstrap,
		DashboardPluginInfo: pluginInfo,
		CORSAllowedOrigins:  cfg.CORSAllowedOrigins,
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("starting ggscale-server", "addr", cfg.HTTPAddr, "env", cfg.Env, "commit", commit)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	cancelWorker()
	select {
	case <-workerDone:
	case <-time.After(30 * time.Second):
		slog.Warn("matchmaker worker did not drain in 30s; forcing shutdown")
	}
	return shutdownErr
}

// startRiverJobs boots the River client (periodic game-session/invite GC) and
// returns a stop function, or nil if River couldn't start. River runs under
// the app DB role via the pool's SET ROLE; its tables are granted in migration
// 0055. Failures are logged and swallowed so a River problem never blocks boot.
func startRiverJobs(ctx context.Context, pool *pgxpool.Pool, appPool *db.Pool, logger *slog.Logger) func() {
	workers := river.NewWorkers()
	river.AddWorker(workers, jobs.NewGameSessionGCWorker(appPool))

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:  logger,
		Workers: workers,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 2},
		},
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(
				river.PeriodicInterval(24*time.Hour),
				func() (river.JobArgs, *river.InsertOpts) { return jobs.GameSessionGCArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true},
			),
		},
	})
	if err != nil {
		logger.Error("river: client init failed; background jobs disabled", "error", err)
		return nil
	}
	if err := client.Start(ctx); err != nil {
		logger.Error("river: start failed; background jobs disabled", "error", err)
		return nil
	}
	logger.Info("river started", "periodic_job", jobs.GameSessionGCKind)
	return func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.Stop(stopCtx); err != nil {
			logger.Warn("river: graceful stop failed", "error", err)
		}
	}
}

func assertAppDBRole(ctx context.Context, pool *pgxpool.Pool) error {
	var currentUser string
	var ownsTenantTables bool
	if err := pool.QueryRow(ctx, `
SELECT current_user,
       EXISTS (
           SELECT 1
           FROM pg_class c
           JOIN pg_namespace n ON n.oid = c.relnamespace
           JOIN pg_roles r ON r.oid = c.relowner
           WHERE n.nspname = 'public'
             AND c.relkind IN ('r', 'p')
             AND c.relname IN ('tenants', 'projects', 'api_keys', 'project_players', 'sessions')
             AND r.rolname = current_user
       )`).Scan(&currentUser, &ownsTenantTables); err != nil {
		return fmt.Errorf("db role assertion: %w", err)
	}
	if currentUser != "ggscale_app" {
		return fmt.Errorf("db role assertion: current_user is %q, want ggscale_app", currentUser)
	}
	if ownsTenantTables {
		return fmt.Errorf("db role assertion: ggscale_app must not own tenant tables")
	}
	return nil
}

// buildFleet wires the configured fleet backend. Returns (nil, nil, nil) when
// the operator hasn't configured one yet — the server still boots and the
// matchmaker (M6) will surface a "not implemented" error to callers. Real
// startup failures (invalid backend name, docker reachable but missing
// image, plugin binary missing, …) return a non-nil error and abort startup.
//
// The optional io.Closer is non-nil for plugin backends; the caller defers
// Close() so the subprocess is reaped on shutdown. In-process backends
// (docker, agones) return a nil closer.
func buildFleet(cfg *config.Config, pool *db.Pool, logger *slog.Logger) (*fleet.Manager, io.Closer, error) {
	if !cfg.FeatureFleetEnabled {
		logger.Warn("fleet disabled: FEATURE_FLEET_ENABLED=false; matchmaker will reject Allocate")
		return nil, nil, nil
	}
	if cfg.FleetBackend == "" {
		logger.Warn("fleet disabled: FLEET_BACKEND unset; matchmaker will reject Allocate until a backend + fleet template are configured")
		return nil, nil, nil
	}

	nanoCPUs := int64(cfg.DockerDefaultCPUs * 1e9)
	backend, err := fleetbuild.New(fleetbuild.Config{
		Backend:                 cfg.FleetBackend,
		Region:                  cfg.FleetRegion,
		PluginDir:               cfg.FleetPluginDir,
		GameServerIP:            cfg.GameServerPublicIP,
		DockerHost:              cfg.DockerHost,
		AgonesNS:                cfg.AgonesNamespace,
		AgonesKubecfg:           cfg.AgonesKubeconfig,
		K3sAPIURL:               cfg.K3sAPIURL,
		K3sSAToken:              cfg.K3sSAToken,
		K3sCACertB64:            cfg.K3sCACertB64,
		DockerBindIP:            cfg.DockerBindIP,
		DockerDefaultMemory:     cfg.DockerDefaultMemory,
		DockerDefaultNanoCPUs:   nanoCPUs,
		DockerDefaultPids:       cfg.DockerDefaultPids,
		DockerRegistryAllowlist: cfg.DockerRegistryAllowlist,
		DockerRequireDigest:     cfg.DockerRequireDigest,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("fleet: %w", err)
	}

	var closer io.Closer
	if c, ok := backend.(io.Closer); ok {
		closer = c
	}
	return fleet.NewManager(
		fleet.NewPostgresStore(pool),
		fleet.NewPostgresFleetStore(pool),
		backend,
		fleet.ManagerOptions{Retries: 3},
	), closer, nil
}

// pluginInfoFromCloser returns a snapshot closure for the dashboard's
// admin/plugins page when the fleet backend is a plugin supervisor. Returns
// nil for non-plugin backends (docker, agones), in which case the page
// renders "no plugin backend configured".
func pluginInfoFromCloser(ctx context.Context, mgr *fleet.Manager, closer io.Closer) func() *dashboard.PluginSnapshot {
	sup, ok := closer.(*fleetplugin.Supervisor)
	if !ok {
		return nil
	}
	return func() *dashboard.PluginSnapshot {
		snap := &dashboard.PluginSnapshot{
			Pid:               sup.Pid(),
			RestartCount:      sup.RestartCount(),
			TotalRestartCount: sup.TotalRestartCount(),
		}
		if mf := sup.Manifest(); mf != nil {
			snap.Name = mf.Name
			snap.Version = mf.Version
			snap.ProtocolVersion = mf.ProtocolVersion
		}
		if mgr != nil {
			probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if err := mgr.Backend().HealthCheck(probeCtx); err != nil {
				snap.HealthErr = err.Error()
			}
		}
		return snap
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(middleware.NewContextHandler(base))
}
