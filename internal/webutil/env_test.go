package webutil_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/webutil"
)

func TestScrubEnvDropsSecretsFromHostAndExtras(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("JWT_SIGNING_KEY", "supersecret")
	t.Setenv("DATABASE_URL", "postgres://...")
	t.Setenv("HOME", "/root")

	out := webutil.ScrubEnv([]string{
		"GAME_MODE=ranked",
		"API_KEY=sneaky", // matches secret regex via "KEY"; must be dropped
		"FOO_TOKEN=x",
	})

	got := joined(out)
	assert.Contains(t, got, "PATH=/usr/bin")
	assert.Contains(t, got, "HOME=/root")
	assert.Contains(t, got, "GAME_MODE=ranked")
	for _, banned := range []string{"JWT_SIGNING_KEY", "DATABASE_URL", "API_KEY", "FOO_TOKEN"} {
		assert.NotContains(t, got, banned+"=", "expected %s to be scrubbed", banned)
	}
}

func TestScrubEnvIncludesAllowlistOnly(t *testing.T) {
	t.Setenv("RANDOM_HOST_VAR", "value")

	out := webutil.ScrubEnv(nil)

	require.NotContains(t, joined(out), "RANDOM_HOST_VAR=", "non-allowlisted vars must be dropped")
}

func joined(env []string) string { return "\n" + strings.Join(env, "\n") + "\n" }
