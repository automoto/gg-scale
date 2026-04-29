package config_test

import (
	"os"
	"path/filepath"
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

	cases := []struct {
		field string
		got   string
		want  string
	}{
		{"HTTPAddr", cfg.HTTPAddr, ":8080"},
		{"ValkeyAddr", cfg.ValkeyAddr, "localhost:6379"},
		{"LogLevel", cfg.LogLevel, "info"},
		{"Env", cfg.Env, "dev"},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			assert.Equal(t, c.want, c.got)
		})
	}
}

func TestLoad_overrides_defaults_when_vars_set(t *testing.T) {
	cases := []struct {
		envVar string
		value  string
		got    func(*config.Config) string
	}{
		{"HTTP_ADDR", ":9090", func(c *config.Config) string { return c.HTTPAddr }},
		{"VALKEY_ADDR", "valkey:6379", func(c *config.Config) string { return c.ValkeyAddr }},
		{"LOG_LEVEL", "debug", func(c *config.Config) string { return c.LogLevel }},
		{"ENV", "staging", func(c *config.Config) string { return c.Env }},
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
	for _, k := range []string{"DATABASE_URL", "HTTP_ADDR", "VALKEY_ADDR", "LOG_LEVEL", "ENV", "JWT_SIGNING_KEY"} {
		t.Setenv(k, "")
	}
}
