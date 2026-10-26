package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/config"
)

func TestLoad_should_default_drain_timeout_to_30s(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	cfg, err := config.Load()

	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, cfg.DrainTimeout)
}

func TestLoadRelay_should_default_drain_timeout_to_zero(t *testing.T) {
	setRelayEnv(t, strings.Repeat("a", 32), "203.0.113.10")

	rc, err := config.LoadRelay()

	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), rc.DrainTimeout)
}

func TestLoad_should_refuse_removed_relay_urls(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("RELAY_URLS", "turn:relay.example.com:3478?transport=udp")

	_, err := config.Load()

	assert.ErrorContains(t, err, "RELAY_URLS was removed")
}

func TestLoad_should_read_relay_probe_token_from_file(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	path := filepath.Join(t.TempDir(), "probe-token")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("p", 32)+"\n"), 0o600))
	t.Setenv("RELAY_PROBE_TOKEN_FILE", path)

	cfg, err := config.Load()

	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("p", 32), cfg.RelayProbeToken)
}

func TestLoad_should_read_platform_admin_token_from_file(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	path := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("a", 32)+"\n"), 0o600))
	t.Setenv("PLATFORM_ADMIN_TOKEN_FILE", path)

	cfg, err := config.Load()

	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("a", 32), cfg.PlatformAdminToken)
}

func TestLoad_should_parse_drain_timeout_as_go_duration(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("DRAIN_TIMEOUT", "1m30s")

	cfg, err := config.Load()

	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, cfg.DrainTimeout)
}

func TestLoadRelay_should_parse_drain_timeout_as_go_duration(t *testing.T) {
	setRelayEnv(t, strings.Repeat("a", 32), "203.0.113.10")
	t.Setenv("DRAIN_TIMEOUT", "45s")

	rc, err := config.LoadRelay()

	require.NoError(t, err)
	assert.Equal(t, 45*time.Second, rc.DrainTimeout)
}

func TestLoad_should_reject_invalid_drain_timeout(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("DRAIN_TIMEOUT", "soon")

	_, err := config.Load()

	assert.ErrorContains(t, err, "DRAIN_TIMEOUT")
}

func TestLoadRelay_should_reject_negative_drain_timeout(t *testing.T) {
	setRelayEnv(t, strings.Repeat("a", 32), "203.0.113.10")
	t.Setenv("DRAIN_TIMEOUT", "-1s")

	_, err := config.LoadRelay()

	assert.ErrorContains(t, err, "DRAIN_TIMEOUT")
}

func TestLoad_should_reject_short_lifecycle_token(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("LIFECYCLE_TOKEN", "short")

	_, err := config.Load()

	assert.ErrorContains(t, err, "LIFECYCLE_TOKEN")
}

func TestLoadRelay_should_reject_short_lifecycle_token(t *testing.T) {
	setRelayEnv(t, strings.Repeat("a", 32), "203.0.113.10")
	t.Setenv("LIFECYCLE_TOKEN", "short")

	_, err := config.LoadRelay()

	assert.ErrorContains(t, err, "LIFECYCLE_TOKEN")
}

func TestLoadRelay_should_read_lifecycle_token_from_file(t *testing.T) {
	setRelayEnv(t, strings.Repeat("a", 32), "203.0.113.10")
	path := filepath.Join(t.TempDir(), "lifecycle-token")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("t", 32)+"\n"), 0o600))
	t.Setenv("LIFECYCLE_TOKEN_FILE", path)

	rc, err := config.LoadRelay()

	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("t", 32), rc.LifecycleToken)
}

func TestLoad_should_read_lifecycle_token(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("LIFECYCLE_TOKEN", strings.Repeat("t", 32))

	cfg, err := config.Load()

	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("t", 32), cfg.LifecycleToken)
}
