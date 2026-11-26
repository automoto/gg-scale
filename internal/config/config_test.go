package config_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/automoto/gg-scale/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_should_reject_an_out_of_range_rate_limit_share(t *testing.T) {
	for _, value := range []string{"0", "-0.5", "1.5", "NaN"} {
		t.Run(value, func(t *testing.T) {
			// Arrange
			clearEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv("RATE_LIMIT_SHARE", value)

			// Act
			_, err := config.Load()

			// Assert
			assert.ErrorContains(t, err, "RATE_LIMIT_SHARE")
		})
	}
}

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
	assert.Equal(t, "local", cfg.AppRegion)
	assert.Equal(t, 1.0, cfg.RateLimitShare)
	assert.True(t, cfg.ControlPanelEnabled)
	assert.Empty(t, cfg.ControlPanelBootstrapTokenFile)
	assert.False(t, cfg.ControlPanelCookieSecure)
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
		{"APP_REGION", "us-east", func(c *config.Config) string { return c.AppRegion }},
		{"CONTROL_PANEL_DISABLED", "true", func(c *config.Config) string {
			return strconv.FormatBool(!c.ControlPanelEnabled)
		}},
		{"CONTROL_PANEL_BOOTSTRAP_TOKEN_FILE", "/tmp/ggscale-bootstrap-token", func(c *config.Config) string {
			return c.ControlPanelBootstrapTokenFile
		}},
		{"CONTROL_PANEL_COOKIE_SECURE", "true", func(c *config.Config) string {
			return strconv.FormatBool(c.ControlPanelCookieSecure)
		}},
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

func TestLoad_rejects_blank_app_region(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("APP_REGION", " ")

	_, err := config.Load()
	assert.ErrorContains(t, err, "APP_REGION")
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

func TestLoad_enables_party_queue_by_default(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.True(t, cfg.PartyEnqueueEnabled)
}

func TestLoad_party_queue_can_be_turned_off(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("PARTY_ENQUEUE_ENABLED", "false")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.False(t, cfg.PartyEnqueueEnabled)
}

func TestLoad_party_code_limits_default(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, []any{100, 10, 15 * time.Minute},
		[]any{cfg.PartyCodeIPMaxFailures, cfg.PartyCodePlayerMaxFailures, cfg.PartyCodeCooldown})
}

func TestLoad_defaults_two_factor_key_to_empty(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Empty(t, cfg.TwoFactorEncKey)
}

func TestLoad_accepts_valid_email_verify_signing_key(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	key := strings.Repeat("cd", 32)
	t.Setenv("EMAIL_VERIFY_SIGNING_KEY", key)

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, key, cfg.EmailVerifySigningKey)
}

func TestLoad_rejects_malformed_email_verify_signing_key(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "not hex", value: "not-hex"},
		{name: "too short", value: "deadbeef"},
		{name: "too long", value: strings.Repeat("ab", 33)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv("EMAIL_VERIFY_SIGNING_KEY", tt.value)

			_, err := config.Load()

			assert.ErrorContains(t, err, "EMAIL_VERIFY_SIGNING_KEY")
		})
	}
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

func TestLoad_defaults_player_delete_grace_period_to_30d(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, 720*time.Hour, cfg.PlayerDeleteGracePeriod)
}

func TestLoad_overrides_player_delete_grace_period(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("PLAYER_DELETE_GRACE_PERIOD", "24h")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, 24*time.Hour, cfg.PlayerDeleteGracePeriod)
}

func TestLoad_rejects_nonpositive_player_delete_grace_period(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("PLAYER_DELETE_GRACE_PERIOD", "0")

	_, err := config.Load()

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "PLAYER_DELETE_GRACE_PERIOD")
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

func TestLoad_migrate_url_defaults_empty_and_parses_when_set(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://app@localhost/test")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.DBMigrateURL, "empty falls back to DATABASE_URL at the call site")

	t.Setenv("DB_MIGRATE_URL", "postgres://owner@localhost/test")
	cfg, err = config.Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://owner@localhost/test", cfg.DBMigrateURL)
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

func TestLoad_should_turn_on_management_api_with_new_variable(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("FEATURE_MANAGEMENT_API_ENABLED", "true")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.True(t, cfg.FeatureManagementAPIEnabled)
}

func TestLoad_should_ignore_old_mcp_feature_variable(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("FEATURE_MCP_ENABLED", "true")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.False(t, cfg.FeatureManagementAPIEnabled)
}

func TestLoad_should_read_management_limits(t *testing.T) {
	cases := []struct {
		envVar string
		got    func(*config.Config) int
	}{
		{"MANAGEMENT_TOKEN_MAX_EXPIRY_DAYS", func(c *config.Config) int { return c.ManagementTokenMaxExpiryDays }},
		{"MANAGEMENT_TOKEN_RATE_PER_SECOND", func(c *config.Config) int { return c.ManagementTokenRatePerSecond }},
		{"MANAGEMENT_TOKEN_RATE_BURST", func(c *config.Config) int { return c.ManagementTokenRateBurst }},
		{"MANAGEMENT_TOKEN_CACHE_MAX_ENTRIES", func(c *config.Config) int { return c.ManagementTokenCacheMaxEntries }},
		{"MANAGEMENT_AUTH_FAILURES_PER_MINUTE", func(c *config.Config) int { return c.ManagementAuthFailuresPerMinute }},
		{"MANAGEMENT_AUTH_FAILURE_BURST", func(c *config.Config) int { return c.ManagementAuthFailureBurst }},
		{"MANAGEMENT_MAX_PROJECT_TOKENS", func(c *config.Config) int { return c.ManagementMaxProjectTokens }},
		{"MANAGEMENT_MAX_PROJECT_API_KEYS", func(c *config.Config) int { return c.ManagementMaxProjectAPIKeys }},
		{"LEADERBOARD_MAX_PER_PROJECT", func(c *config.Config) int { return c.LeaderboardMaxPerProject }},
		{"FLEET_ALLOCATION_MAX_CAPACITY", func(c *config.Config) int { return c.FleetAllocationMaxCapacity }},
		{"FLEET_MAX_LIVE_ALLOCATIONS_PER_PROJECT", func(c *config.Config) int { return c.FleetMaxLiveAllocationsPerProject }},
	}
	for _, c := range cases {
		t.Run(c.envVar, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv(c.envVar, "7")

			cfg, err := config.Load()
			require.NoError(t, err)

			assert.Equal(t, 7, c.got(cfg))
		})
	}
}

func TestLoad_should_keep_default_limit_when_only_old_name_is_set(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("MCP_MAX_PROJECT_TOKENS", "3")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, 20, cfg.ManagementMaxProjectTokens)
}

func TestLoad_should_name_new_variable_when_limit_is_not_positive(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("MANAGEMENT_TOKEN_MAX_EXPIRY_DAYS", "0")

	_, err := config.Load()

	assert.ErrorContains(t, err, "MANAGEMENT_TOKEN_MAX_EXPIRY_DAYS")
}

func TestLoad_should_default_the_management_token_cache_ttl_to_2s(t *testing.T) {
	// Arrange
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	// Act
	cfg, err := config.Load()

	// Assert
	require.NoError(t, err)
	assert.Equal(t, 2*time.Second, cfg.ManagementTokenCacheTTL)
}

func TestLoad_should_accept_a_zero_management_token_cache_ttl(t *testing.T) {
	// Arrange
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("MANAGEMENT_TOKEN_CACHE_TTL", "0s")

	// Act
	cfg, err := config.Load()

	// Assert
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), cfg.ManagementTokenCacheTTL)
}

func TestLoad_should_refuse_a_negative_management_token_cache_ttl(t *testing.T) {
	// Arrange
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("MANAGEMENT_TOKEN_CACHE_TTL", "-1s")

	// Act
	_, err := config.Load()

	// Assert
	assert.ErrorContains(t, err, "MANAGEMENT_TOKEN_CACHE_TTL")
}

func TestLoad_should_default_the_management_token_cache_max_entries_to_10000(t *testing.T) {
	// Arrange
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	// Act
	cfg, err := config.Load()

	// Assert
	require.NoError(t, err)
	assert.Equal(t, 10000, cfg.ManagementTokenCacheMaxEntries)
}

func TestLoad_should_refuse_a_zero_management_token_cache_max_entries(t *testing.T) {
	// Arrange
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("MANAGEMENT_TOKEN_CACHE_MAX_ENTRIES", "0")

	// Act
	_, err := config.Load()

	// Assert
	assert.ErrorContains(t, err, "MANAGEMENT_TOKEN_CACHE_MAX_ENTRIES")
}

func TestLoad_should_not_put_the_limit_value_in_the_error(t *testing.T) {
	// Arrange
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("MANAGEMENT_MAX_PROJECT_API_KEYS", "-7")

	// Act
	_, err := config.Load()

	// Assert
	assert.EqualError(t, err, "MANAGEMENT_MAX_PROJECT_API_KEYS: must be a positive integer")
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
		"DATABASE_URL", "DATABASE_URL_FILE", "HTTP_ADDR", "APP_REGION", "RATE_LIMIT_SHARE", "LOG_LEVEL", "ENV",
		"PARTY_ENQUEUE_ENABLED",
		"PARTY_CODE_IP_MAX_FAILURES", "PARTY_CODE_PLAYER_MAX_FAILURES", "PARTY_CODE_COOLDOWN",
		"JWT_SIGNING_KEY", "JWT_SIGNING_KEY_FILE",
		"EMAIL_VERIFY_SIGNING_KEY", "EMAIL_VERIFY_SIGNING_KEY_FILE",
		"TWO_FACTOR_ENC_KEY", "TWO_FACTOR_ENC_KEY_FILE",
		"CONTROL_PANEL_DISABLED", "CONTROL_PANEL_BOOTSTRAP_TOKEN_FILE", "CONTROL_PANEL_COOKIE_SECURE",
		"RELAY_URLS", "RELAY_PROBE_TOKEN", "RELAY_PROBE_TOKEN_FILE", "PLATFORM_ADMIN_TOKEN", "PLATFORM_ADMIN_TOKEN_FILE",
		"FEATURE_MANAGEMENT_API_ENABLED", "MANAGEMENT_TOKEN_MAX_EXPIRY_DAYS", "MANAGEMENT_MAX_PROJECT_TOKENS",
		"LEADERBOARD_MAX_PER_PROJECT", "FLEET_ALLOCATION_MAX_CAPACITY", "FLEET_MAX_LIVE_ALLOCATIONS_PER_PROJECT",
	} {
		t.Setenv(k, "")
	}
}

func TestLoad_should_default_leaderboard_max_per_project_to_1000(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, 1000, cfg.LeaderboardMaxPerProject)
}

func TestLoad_should_refuse_leaderboard_max_per_project_of_zero(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("LEADERBOARD_MAX_PER_PROJECT", "0")

	_, err := config.Load()

	assert.ErrorContains(t, err, "LEADERBOARD_MAX_PER_PROJECT")
}

func TestLoad_should_default_fleet_allocation_max_capacity_to_100(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, 100, cfg.FleetAllocationMaxCapacity)
}

func TestLoad_should_refuse_fleet_allocation_max_capacity_of_zero(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("FLEET_ALLOCATION_MAX_CAPACITY", "0")

	_, err := config.Load()

	assert.ErrorContains(t, err, "FLEET_ALLOCATION_MAX_CAPACITY")
}

func TestLoad_should_default_fleet_max_live_allocations_per_project_to_100(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, 100, cfg.FleetMaxLiveAllocationsPerProject)
}

func TestLoad_should_refuse_fleet_max_live_allocations_per_project_of_zero(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("FLEET_MAX_LIVE_ALLOCATIONS_PER_PROJECT", "0")

	_, err := config.Load()

	assert.ErrorContains(t, err, "FLEET_MAX_LIVE_ALLOCATIONS_PER_PROJECT")
}

func TestLoad_should_default_agones_project_isolation_to_off(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("AGONES_PROJECT_ISOLATION", "")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.False(t, cfg.AgonesProjectIsolation)
}

func TestLoad_should_read_agones_project_isolation(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("AGONES_PROJECT_ISOLATION", "true")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.True(t, cfg.AgonesProjectIsolation)
}

func TestLoad_should_refuse_an_agones_project_isolation_that_is_not_a_bool(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("AGONES_PROJECT_ISOLATION", "maybe")

	_, err := config.Load()

	assert.ErrorContains(t, err, "AGONES_PROJECT_ISOLATION")
}

func TestLoad_should_default_the_cache_times(t *testing.T) {
	// Arrange
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	// Act
	cfg, err := config.Load()

	// Assert
	require.NoError(t, err)
	assert.Equal(t, map[string]time.Duration{
		"API_KEY_CACHE_TTL":             30 * time.Second,
		"FEATURE_GRANT_CACHE_TTL":       5 * time.Second,
		"RBAC_POLICY_RELOAD_INTERVAL":   10 * time.Second,
		"RATE_LIMIT_OVERRIDE_CACHE_TTL": 5 * time.Second,
		"STORAGE_LIMIT_CACHE_TTL":       5 * time.Second,
		"LEADERBOARD_TOP_CACHE_TTL":     10 * time.Second,
		"CORS_ORIGIN_CACHE_TTL":         30 * time.Second,
	}, map[string]time.Duration{
		"API_KEY_CACHE_TTL":             cfg.APIKeyCacheTTL,
		"FEATURE_GRANT_CACHE_TTL":       cfg.FeatureGrantCacheTTL,
		"RBAC_POLICY_RELOAD_INTERVAL":   cfg.RBACPolicyReloadInterval,
		"RATE_LIMIT_OVERRIDE_CACHE_TTL": cfg.RateLimitOverrideCacheTTL,
		"STORAGE_LIMIT_CACHE_TTL":       cfg.StorageLimitCacheTTL,
		"LEADERBOARD_TOP_CACHE_TTL":     cfg.LeaderboardTopCacheTTL,
		"CORS_ORIGIN_CACHE_TTL":         cfg.CORSOriginCacheTTL,
	})
}

func TestLoad_should_read_a_cache_time_from_the_env(t *testing.T) {
	// Arrange
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("API_KEY_CACHE_TTL", "7s")

	// Act
	cfg, err := config.Load()

	// Assert
	require.NoError(t, err)
	assert.Equal(t, 7*time.Second, cfg.APIKeyCacheTTL)
}

func TestLoad_should_refuse_a_cache_time_of_zero(t *testing.T) {
	for _, name := range []string{
		"API_KEY_CACHE_TTL",
		"FEATURE_GRANT_CACHE_TTL",
		"RBAC_POLICY_RELOAD_INTERVAL",
		"RATE_LIMIT_OVERRIDE_CACHE_TTL",
		"STORAGE_LIMIT_CACHE_TTL",
		"LEADERBOARD_TOP_CACHE_TTL",
		"CORS_ORIGIN_CACHE_TTL",
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			clearEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv(name, "0s")

			// Act
			_, err := config.Load()

			// Assert
			assert.EqualError(t, err, name+": must be a positive duration such as 30s")
		})
	}
}

func TestLoad_should_keep_a_malformed_cache_time_out_of_the_error(t *testing.T) {
	for _, name := range []string{
		"API_KEY_CACHE_TTL",
		"FEATURE_GRANT_CACHE_TTL",
		"RBAC_POLICY_RELOAD_INTERVAL",
		"RATE_LIMIT_OVERRIDE_CACHE_TTL",
		"STORAGE_LIMIT_CACHE_TTL",
		"LEADERBOARD_TOP_CACHE_TTL",
		"CORS_ORIGIN_CACHE_TTL",
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			clearEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv(name, "pasted-secret")

			// Act
			_, err := config.Load()

			// Assert
			assert.EqualError(t, err, name+": must be a positive duration such as 30s")
		})
	}
}
