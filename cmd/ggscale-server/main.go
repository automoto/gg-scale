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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/ggscale/ggscale/internal/auth"
	cachebuild "github.com/ggscale/ggscale/internal/cache/build"
	"github.com/ggscale/ggscale/internal/config"
	"github.com/ggscale/ggscale/internal/dashboard"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	fleetbuild "github.com/ggscale/ggscale/internal/fleet/build"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/mailer"
	_ "github.com/ggscale/ggscale/internal/mailer/noop"
	_ "github.com/ggscale/ggscale/internal/mailer/smtp"
	"github.com/ggscale/ggscale/internal/matchmaker"
	"github.com/ggscale/ggscale/internal/middleware"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/realtime"
	"github.com/ggscale/ggscale/internal/relay"
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

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	poolCfg.ConnConfig.Tracer = observability.NewPgxTracer(registry)
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return err
	}
	defer pool.Close()

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

	m, err := mailer.New(cfg.MailProvider, cfg.SMTPAddr, cfg.SMTPUser, cfg.SMTPPassword, cfg.MailFrom)
	if err != nil {
		return fmt.Errorf("mailer: %w", err)
	}

	appPool := db.NewPool(pool)
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

	hub := realtime.NewHub()

	var relayIssuer *relay.Issuer
	if cfg.RelaySharedSecret != "" {
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

	mmQueue := matchmaker.NewPGQueue(appPool)
	if fleetMgr != nil {
		worker := matchmaker.NewWorker(mmQueue, fleetMgr, hub, matchmaker.WorkerConfig{
			BucketSize: cfg.MatchmakerBucketSize,
			Interval:   cfg.MatchmakerInterval,
			Logger:     logger,
		})
		workerCtx, cancelWorker := context.WithCancel(ctx)
		defer cancelWorker()
		go worker.Run(workerCtx)
	} else {
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
		Fleet:                fleetMgr,
		Hub:                  hub,
		RealtimeMaxPerTenant: cfg.RealtimeMaxPerTenant,
		Matchmaker:           mmQueue,
		RelayIssuer:          relayIssuer,
		Dashboard: dashboard.Config{
			Mount:        cfg.DashboardEnabled,
			CookieSecure: cfg.DashboardCookieSecure,
		},
		DashboardBootstrap: dashboardBootstrap,
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
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
	return srv.Shutdown(shutdownCtx)
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
	if cfg.FleetBackend == "docker" && cfg.DockerGameServerImage == "" {
		logger.Warn("fleet disabled: DOCKER_GAMESERVER_IMAGE unset; matchmaker will reject Allocate")
		return nil, nil, nil
	}

	backend, err := fleetbuild.New(fleetbuild.Config{
		Backend:       cfg.FleetBackend,
		Region:        cfg.FleetRegion,
		PluginDir:     cfg.FleetPluginDir,
		GameServerIP:  cfg.GameServerPublicIP,
		DockerImage:   cfg.DockerGameServerImage,
		DockerPort:    cfg.DockerGameServerPort,
		DockerProbe:   cfg.DockerProbeType,
		DockerProbeP:  cfg.DockerProbePath,
		DockerMaxSess: cfg.DockerMaxSessions,
		DockerHost:    cfg.DockerHost,
		AgonesNS:      cfg.AgonesNamespace,
		AgonesFleet:   cfg.AgonesFleetName,
		AgonesLabels:  cfg.AgonesSelectorLabels,
		AgonesKubecfg: cfg.AgonesKubeconfig,
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
		backend,
		fleet.ManagerOptions{Retries: 3},
	), closer, nil
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
