package config_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
	assert.True(t, cfg.ControlPanelEnabled)
	assert.Empty(t, cfg.ControlPanelBootstrapTokenFile)
	assert.False(t, cfg.ControlPanelCookieSecure)
	assert.Equal(t, "memory", cfg.CacheBackend)
	assert.Equal(t, "127.0.0.1", cfg.CacheOlricBindAddr)
	assert.Equal(t, 3320, cfg.CacheOlricBindPort)
	assert.Equal(t, "127.0.0.1", cfg.CacheOlricMemberlistAddr)
	assert.Equal(t, 3322, cfg.CacheOlricMemberlistPort)
	assert.Empty(t, cfg.CacheOlricPeers)
	assert.Equal(t, 1, cfg.CacheOlricReplicaCount)
	assert.Empty(t, cfg.TrustedProxyCIDRs)
	assert.Empty(t, cfg.FleetBackend)
	assert.False(t, cfg.FeatureFleetEnabled)
	assert.False(t, cfg.FeatureP2PRelayEnabled)
}

func TestLoad_feature_switches_parse_true(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("FEATURE_FLEET_ENABLED", "true")
	t.Setenv("FEATURE_P2P_RELAY_ENABLED", "true")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.True(t, cfg.FeatureFleetEnabled)
	assert.True(t, cfg.FeatureP2PRelayEnabled)
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
		{"CONTROL_PANEL_DISABLED", "true", func(c *config.Config) string {
			return strconv.FormatBool(!c.ControlPanelEnabled)
		}},
		{"CONTROL_PANEL_BOOTSTRAP_TOKEN_FILE", "/tmp/ggscale-bootstrap-token", func(c *config.Config) string {
			return c.ControlPanelBootstrapTokenFile
		}},
		{"CONTROL_PANEL_COOKIE_SECURE", "true", func(c *config.Config) string {
			return strconv.FormatBool(c.ControlPanelCookieSecure)
		}},
		{"CACHE_BACKEND", "olric", func(c *config.Config) string { return c.CacheBackend }},
		{"CACHE_OLRIC_BIND_ADDR", "0.0.0.0", func(c *config.Config) string { return c.CacheOlricBindAddr }},
		{"CACHE_OLRIC_REPLICA_COUNT", "2", func(c *config.Config) string { return strconv.Itoa(c.CacheOlricReplicaCount) }},
		{"TRUSTED_PROXY_HEADER", "CF-Connecting-IP", func(c *config.Config) string { return c.TrustedProxyHeader }},
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

func TestLoad_parses_trusted_proxy_cidrs(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.0/8, 192.0.2.0/24")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, []string{"10.0.0.0/8", "192.0.2.0/24"}, cfg.TrustedProxyCIDRs)
}

func TestLoad_rejects_invalid_trusted_proxy_cidr(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("TRUSTED_PROXY_CIDRS", "not-a-cidr")

	_, err := config.Load()

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "TRUSTED_PROXY_CIDRS")
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

func TestLoad_accepts_valid_two_factor_key(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	key := strings.Repeat("ab", 32)
	t.Setenv("TWO_FACTOR_ENC_KEY", key)

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, key, cfg.TwoFactorEncKey)
}

func TestLoad_defaults_two_factor_key_to_empty(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Empty(t, cfg.TwoFactorEncKey)
}

func TestLoad_rejects_malformed_two_factor_key(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{name: "should_reject_non_hex", value: "not-hex-at-all"},
		{name: "should_reject_short_key", value: "abcd1234"},
		{name: "should_reject_long_key", value: strings.Repeat("ab", 33)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv("TWO_FACTOR_ENC_KEY", c.value)

			_, err := config.Load()

			require.Error(t, err)
			assert.Contains(t, err.Error(), "TWO_FACTOR_ENC_KEY")
		})
	}
}

func TestLoad_docker_require_digest_is_strict_bool(t *testing.T) {
	t.Run("rejects_yes_no", func(t *testing.T) {
		for _, v := range []string{"yes", "no"} {
			clearEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv("DOCKER_REQUIRE_DIGEST", v)

			_, err := config.Load()

			require.Error(t, err)
			assert.Contains(t, err.Error(), "DOCKER_REQUIRE_DIGEST")
		}
	})
	t.Run("accepts_1_and_true", func(t *testing.T) {
		for _, v := range []string{"1", "true"} {
			clearEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv("DOCKER_REQUIRE_DIGEST", v)

			cfg, err := config.Load()

			require.NoError(t, err)
			assert.True(t, cfg.DockerRequireDigest)
		}
	})
}

func TestLoad_defaults_db_max_conn_idle_time_to_10m(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, 10*time.Minute, cfg.DBMaxConnIdleTime)
}

func TestLoad_overrides_db_max_conn_idle_time(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("DB_MAX_CONN_IDLE_TIME", "30s")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, 30*time.Second, cfg.DBMaxConnIdleTime)
}

func TestLoad_rejects_nonpositive_db_max_conn_idle_time(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("DB_MAX_CONN_IDLE_TIME", "0")

	_, err := config.Load()

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DB_MAX_CONN_IDLE_TIME")
}

func TestLoad_read_pool_off_by_default(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Empty(t, cfg.DBReadURL)
	assert.Equal(t, 25, cfg.DBReadMaxConns)
}

func TestLoad_reads_db_read_url_when_set(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("DB_READ_URL", "postgres://replica/test")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, "postgres://replica/test", cfg.DBReadURL)
}

func TestLoad_rejects_small_read_pool_when_read_url_set(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("DB_READ_URL", "postgres://replica/test")
	t.Setenv("DB_READ_MAX_CONNS", "2")

	_, err := config.Load()

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DB_READ_MAX_CONNS")
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
		"TWO_FACTOR_ENC_KEY", "TWO_FACTOR_ENC_KEY_FILE",
		"CONTROL_PANEL_DISABLED", "CONTROL_PANEL_BOOTSTRAP_TOKEN_FILE", "CONTROL_PANEL_COOKIE_SECURE",
		"CACHE_BACKEND", "CACHE_OLRIC_BIND_ADDR", "CACHE_OLRIC_BIND_PORT",
		"CACHE_OLRIC_MEMBERLIST_ADDR", "CACHE_OLRIC_MEMBERLIST_PORT",
		"CACHE_OLRIC_PEERS", "CACHE_OLRIC_REPLICA_COUNT",
	} {
		t.Setenv(k, "")
	}
}
