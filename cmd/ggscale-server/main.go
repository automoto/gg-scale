// Command ggscale-server is the ggscale control-plane HTTP server.
package main

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/mailer"
	_ "github.com/ggscale/ggscale/internal/mailer/noop"
	_ "github.com/ggscale/ggscale/internal/mailer/smtp"
	"github.com/ggscale/ggscale/internal/middleware"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/ratelimit"
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

	router := httpapi.NewRouter(httpapi.Deps{
		Version:  "v1",
		Commit:   commit,
		Pool:     db.NewPool(pool),
		Lookup:   tenant.NewSQLLookup(pool),
		Limiter:  ratelimit.NewCacheLimiter(store),
		Signer:   signer,
		Mailer:   m,
		MailFrom: cfg.MailFrom,
		Cache:    store,
		Registry: registry,
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
