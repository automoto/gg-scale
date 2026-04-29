// Package config loads runtime configuration from environment variables.
// All declared vars must also appear in .env.example; the drift test in
// config_test.go enforces this contract.
package config

import (
	"fmt"
	"os"
)

// Config holds runtime configuration loaded from the environment.
type Config struct {
	HTTPAddr      string
	DatabaseURL   string
	ValkeyAddr    string
	LogLevel      string
	Env           string
	JWTSigningKey string
}

type varDecl struct {
	name     string
	required bool
	defval   string
	set      func(*Config, string)
}

var declared = []varDecl{
	{name: "DATABASE_URL", required: true, set: func(c *Config, v string) { c.DatabaseURL = v }},
	{name: "HTTP_ADDR", defval: ":8080", set: func(c *Config, v string) { c.HTTPAddr = v }},
	{name: "VALKEY_ADDR", defval: "localhost:6379", set: func(c *Config, v string) { c.ValkeyAddr = v }},
	{name: "LOG_LEVEL", defval: "info", set: func(c *Config, v string) { c.LogLevel = v }},
	{name: "ENV", defval: "dev", set: func(c *Config, v string) { c.Env = v }},
	{name: "JWT_SIGNING_KEY", set: func(c *Config, v string) { c.JWTSigningKey = v }},
}

// Load reads the environment and returns a populated Config or an error if
// any required variable is missing.
func Load() (*Config, error) {
	cfg := &Config{}
	for _, v := range declared {
		val := os.Getenv(v.name)
		if val == "" {
			if v.required {
				return nil, fmt.Errorf("required env var %s is missing", v.name)
			}
			val = v.defval
		}
		v.set(cfg, val)
	}
	return cfg, nil
}

// DeclaredVars returns the list of env-var names this package reads.
// Used by the drift test to compare against .env.example.
func DeclaredVars() []string {
	out := make([]string, len(declared))
	for i, v := range declared {
		out[i] = v.name
	}
	return out
}
