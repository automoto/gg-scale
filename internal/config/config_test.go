package config_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ggscale/ggscale/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_returns_error_when_required_var_missing(t *testing.T) {
	clearEnv(t)

	_, err := config.Load()

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE_URL")
}

func TestLoad_uses_defaults_when_optional_vars_missing(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, ":8080", cfg.HTTPAddr)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, "dev", cfg.Env)
	assert.True(t, cfg.DashboardEnabled)
	assert.Empty(t, cfg.DashboardBootstrapTokenFile)
	assert.False(t, cfg.DashboardCookieSecure)
	assert.Equal(t, "memory", cfg.CacheBackend)
	assert.Equal(t, "127.0.0.1", cfg.CacheOlricBindAddr)
	assert.Equal(t, 3320, cfg.CacheOlricBindPort)
	assert.Equal(t, "127.0.0.1", cfg.CacheOlricMemberlistAddr)
	assert.Equal(t, 3322, cfg.CacheOlricMemberlistPort)
	assert.Empty(t, cfg.CacheOlricPeers)
}

func TestLoad_overrides_defaults_when_vars_set(t *testing.T) {
	cases := []struct {
		envVar string
		value  string
		got    func(*config.Config) string
	}{
		{"HTTP_ADDR", ":9090", func(c *config.Config) string { return c.HTTPAddr }},
		{"LOG_LEVEL", "debug", func(c *config.Config) string { return c.LogLevel }},
		{"ENV", "staging", func(c *config.Config) string { return c.Env }},
		{"DASHBOARD_DISABLED", "true", func(c *config.Config) string {
			return strconv.FormatBool(!c.DashboardEnabled)
		}},
		{"DASHBOARD_BOOTSTRAP_TOKEN_FILE", "/tmp/ggscale-bootstrap-token", func(c *config.Config) string {
			return c.DashboardBootstrapTokenFile
		}},
		{"DASHBOARD_COOKIE_SECURE", "true", func(c *config.Config) string {
			return strconv.FormatBool(c.DashboardCookieSecure)
		}},
		{"CACHE_BACKEND", "olric", func(c *config.Config) string { return c.CacheBackend }},
		{"CACHE_OLRIC_BIND_ADDR", "0.0.0.0", func(c *config.Config) string { return c.CacheOlricBindAddr }},
	}
	for _, c := range cases {
		t.Run(c.envVar, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv(c.envVar, c.value)

			cfg, err := config.Load()
			require.NoError(t, err)

			assert.Equal(t, c.value, c.got(cfg))
		})
	}
}

func TestLoad_rejects_unknown_cache_backend(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("CACHE_BACKEND", "nope")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "CACHE_BACKEND")
}

func TestLoad_parses_olric_peers_csv(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("CACHE_OLRIC_PEERS", "node-1:3322, node-2:3322 ,node-3:3322")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, []string{"node-1:3322", "node-2:3322", "node-3:3322"}, cfg.CacheOlricPeers)
}

func TestLoad_reads_DATABASE_URL_FILE_when_set(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "db_url")
	require.NoError(t, os.WriteFile(path, []byte("postgres://localhost/from-file\n"), 0o600))
	t.Setenv("DATABASE_URL_FILE", path)

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, "postgres://localhost/from-file", cfg.DatabaseURL)
}

func TestLoad_FILE_wins_over_plain_env(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "db_url")
	require.NoError(t, os.WriteFile(path, []byte("postgres://localhost/from-file"), 0o600))
	t.Setenv("DATABASE_URL", "postgres://localhost/from-env")
	t.Setenv("DATABASE_URL_FILE", path)

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, "postgres://localhost/from-file", cfg.DatabaseURL)
}

func TestLoad_returns_error_when_FILE_path_is_unreadable(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL_FILE", "/nonexistent/path/to/secret")

	_, err := config.Load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE_URL_FILE")
}

func TestEnvExample_has_no_drift(t *testing.T) {
	declared := config.DeclaredVars()

	exampleBytes, err := os.ReadFile(filepath.Join("..", "..", ".env.example"))
	require.NoError(t, err)

	exampleVars := parseEnvFileKeys(string(exampleBytes))

	for _, v := range declared {
		assert.Contains(t, exampleVars, v, "declared var %q missing from .env.example", v)
	}
	for _, v := range exampleVars {
		assert.Contains(t, declared, v, "var %q in .env.example has no declaration in config struct", v)
	}
}

func parseEnvFileKeys(content string) []string {
	var keys []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		keys = append(keys, line[:eq])
	}
	return keys
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"DATABASE_URL", "DATABASE_URL_FILE", "HTTP_ADDR", "LOG_LEVEL", "ENV",
		"JWT_SIGNING_KEY", "JWT_SIGNING_KEY_FILE",
		"DASHBOARD_DISABLED", "DASHBOARD_BOOTSTRAP_TOKEN_FILE", "DASHBOARD_COOKIE_SECURE",
		"CACHE_BACKEND", "CACHE_OLRIC_BIND_ADDR", "CACHE_OLRIC_BIND_PORT",
		"CACHE_OLRIC_MEMBERLIST_ADDR", "CACHE_OLRIC_MEMBERLIST_PORT",
		"CACHE_OLRIC_PEERS",
	} {
		t.Setenv(k, "")
	}
}
