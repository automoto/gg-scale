package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/config"
)

func setRelayEnv(t *testing.T, secret, publicIP string) {
	t.Helper()
	t.Setenv("RELAY_SHARED_SECRET", secret)
	t.Setenv("RELAY_PUBLIC_IP", publicIP)
}

func TestLoadRelayParsesDefaults(t *testing.T) {
	setRelayEnv(t, strings.Repeat("a", 32), "203.0.113.10")

	rc, err := config.LoadRelay()

	require.NoError(t, err)
	assert.Equal(t, "203.0.113.10", rc.PublicIP)
	assert.Equal(t, "0.0.0.0", rc.BindAddr)
	assert.Equal(t, 3478, rc.UDPPort)
	assert.Equal(t, "ggscale", rc.Realm)
	assert.Equal(t, 5*time.Minute, rc.CredTTL)
}

func TestLoadRelayRejectsShortSecret(t *testing.T) {
	setRelayEnv(t, "short", "203.0.113.10")

	_, err := config.LoadRelay()

	assert.ErrorContains(t, err, "RELAY_SHARED_SECRET")
}

func TestLoadRelayRejectsMissingPublicIP(t *testing.T) {
	t.Setenv("RELAY_SHARED_SECRET", strings.Repeat("a", 32))

	_, err := config.LoadRelay()

	assert.ErrorContains(t, err, "RELAY_PUBLIC_IP")
}

func TestLoadRelayRejectsBadPublicIP(t *testing.T) {
	setRelayEnv(t, strings.Repeat("a", 32), "not-an-ip")

	_, err := config.LoadRelay()

	assert.ErrorContains(t, err, "RELAY_PUBLIC_IP")
}

func TestLoadRelayRejectsIPv6PublicIP(t *testing.T) {
	setRelayEnv(t, strings.Repeat("a", 32), "2001:db8::1")

	_, err := config.LoadRelay()

	assert.ErrorContains(t, err, "IPv4")
}

func TestLoadRelayDefaultsMaxAllocations(t *testing.T) {
	setRelayEnv(t, strings.Repeat("a", 32), "203.0.113.10")

	rc, err := config.LoadRelay()

	require.NoError(t, err)
	assert.Equal(t, 1000, rc.MaxAllocations)
}

func TestLoadRelayAcceptsPortRange(t *testing.T) {
	setRelayEnv(t, strings.Repeat("a", 32), "203.0.113.10")
	t.Setenv("RELAY_MIN_PORT", "49200")
	t.Setenv("RELAY_MAX_PORT", "49300")

	rc, err := config.LoadRelay()

	require.NoError(t, err)
	assert.Equal(t, 49200, rc.MinPort)
	assert.Equal(t, 49300, rc.MaxPort)
}

func TestLoadRelayRejectsHalfPortRange(t *testing.T) {
	setRelayEnv(t, strings.Repeat("a", 32), "203.0.113.10")
	t.Setenv("RELAY_MIN_PORT", "49200")

	_, err := config.LoadRelay()

	assert.ErrorContains(t, err, "RELAY_MAX_PORT")
}

func TestLoadRelayRejectsInvertedPortRange(t *testing.T) {
	setRelayEnv(t, strings.Repeat("a", 32), "203.0.113.10")
	t.Setenv("RELAY_MIN_PORT", "49300")
	t.Setenv("RELAY_MAX_PORT", "49200")

	_, err := config.LoadRelay()

	assert.ErrorContains(t, err, "RELAY_MIN_PORT")
}
