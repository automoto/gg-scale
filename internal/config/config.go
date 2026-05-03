// Package config loads runtime configuration from environment variables.
// All declared vars must also appear in .env.example; the drift test in
// config_test.go enforces this contract.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds runtime configuration loaded from the environment.
type Config struct {
	HTTPAddr      string
	DatabaseURL   string
	LogLevel      string
	Env           string
	JWTSigningKey string

	// DashboardEnabled controls whether /v1/dashboard is mounted.
	DashboardEnabled bool
	// DashboardBootstrapTokenFile optionally writes the first-run token to a file.
	DashboardBootstrapTokenFile string
	// DashboardCookieSecure sets the Secure flag on the dashboard session cookie.
	DashboardCookieSecure bool

	// Cache backend selection. CacheBackend is one of "memory" or "olric".
	// "memory" is the default and is appropriate for single-process
	// self-host. "olric" links every app process into an embedded Olric
	// cluster (or, with non-empty CacheOlricPeers, a multi-node cluster
	// joined via memberlist gossip).
	CacheBackend string
	// CacheOlricBindAddr is the Olric-protocol bind address. Default
	// "127.0.0.1". Only consulted when CacheBackend is "olric".
	CacheOlricBindAddr string
	// CacheOlricBindPort is the Olric-protocol port. 0 picks an ephemeral
	// port. Default 3320.
	CacheOlricBindPort int
	// CacheOlricMemberlistAddr is the gossip bind address. Default
	// matches CacheOlricBindAddr.
	CacheOlricMemberlistAddr string
	// CacheOlricMemberlistPort is the gossip port. 0 picks an ephemeral
	// port. Default 3322.
	CacheOlricMemberlistPort int
	// CacheOlricPeers is the comma-separated host:port list of memberlist
	// endpoints to join. Empty means a cluster of one.
	CacheOlricPeers []string

	// Mail provider and connection settings.
	// MailProvider selects the registered provider: "smtp" (default) or "noop".
	// External providers register via mailer.Register in their init().
	MailProvider string
	// SMTPAddr is the SMTP server address (host:port). Default "localhost:1025"
	// matches MailHog in the dev compose stack.
	SMTPAddr string
	// SMTPUser is the SMTP username. Leave empty for unauthenticated relays
	// (e.g. MailHog in dev).
	SMTPUser string
	// SMTPPassword is the SMTP password. Unused when SMTPUser is empty.
	SMTPPassword string
	// MailFrom is the From address on outbound mail. Default "noreply@ggscale.dev".
	MailFrom string
}

type varDecl struct {
	name     string
	required bool
	defval   string
	set      func(*Config, string) error
}

var declared = []varDecl{
	{name: "DATABASE_URL", required: true, set: func(c *Config, v string) error { c.DatabaseURL = v; return nil }},
	{name: "HTTP_ADDR", defval: ":8080", set: func(c *Config, v string) error { c.HTTPAddr = v; return nil }},
	{name: "LOG_LEVEL", defval: "info", set: func(c *Config, v string) error { c.LogLevel = v; return nil }},
	{name: "ENV", defval: "dev", set: func(c *Config, v string) error { c.Env = v; return nil }},
	{name: "JWT_SIGNING_KEY", set: func(c *Config, v string) error { c.JWTSigningKey = v; return nil }},
	{name: "DASHBOARD_DISABLED", defval: "false", set: func(c *Config, v string) error {
		disabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("DASHBOARD_DISABLED %q: %w", v, err)
		}
		c.DashboardEnabled = !disabled
		return nil
	}},
	{name: "DASHBOARD_BOOTSTRAP_TOKEN_FILE", defval: "", set: func(c *Config, v string) error {
		c.DashboardBootstrapTokenFile = v
		return nil
	}},
	{name: "DASHBOARD_COOKIE_SECURE", defval: "false", set: func(c *Config, v string) error {
		secure, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("DASHBOARD_COOKIE_SECURE %q: %w", v, err)
		}
		c.DashboardCookieSecure = secure
		return nil
	}},

	{name: "CACHE_BACKEND", defval: "memory", set: func(c *Config, v string) error {
		switch v {
		case "memory", "olric":
			c.CacheBackend = v
			return nil
		default:
			return fmt.Errorf("CACHE_BACKEND %q: must be one of memory|olric", v)
		}
	}},
	{name: "CACHE_OLRIC_BIND_ADDR", defval: "127.0.0.1", set: func(c *Config, v string) error {
		c.CacheOlricBindAddr = v
		return nil
	}},
	{name: "CACHE_OLRIC_BIND_PORT", defval: "3320", set: func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("CACHE_OLRIC_BIND_PORT %q: %w", v, err)
		}
		c.CacheOlricBindPort = n
		return nil
	}},
	{name: "CACHE_OLRIC_MEMBERLIST_ADDR", defval: "127.0.0.1", set: func(c *Config, v string) error {
		c.CacheOlricMemberlistAddr = v
		return nil
	}},
	{name: "CACHE_OLRIC_MEMBERLIST_PORT", defval: "3322", set: func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("CACHE_OLRIC_MEMBERLIST_PORT %q: %w", v, err)
		}
		c.CacheOlricMemberlistPort = n
		return nil
	}},
	{name: "CACHE_OLRIC_PEERS", defval: "", set: func(c *Config, v string) error {
		c.CacheOlricPeers = splitCSV(v)
		return nil
	}},

	{name: "MAIL_PROVIDER", defval: "smtp", set: func(c *Config, v string) error {
		c.MailProvider = v
		return nil
	}},
	{name: "SMTP_ADDR", defval: "localhost:1025", set: func(c *Config, v string) error {
		c.SMTPAddr = v
		return nil
	}},
	{name: "SMTP_USER", defval: "", set: func(c *Config, v string) error {
		c.SMTPUser = v
		return nil
	}},
	{name: "SMTP_PASSWORD", defval: "", set: func(c *Config, v string) error {
		c.SMTPPassword = v
		return nil
	}},
	{name: "MAIL_FROM", defval: "noreply@ggscale.dev", set: func(c *Config, v string) error {
		c.MailFrom = v
		return nil
	}},
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
		if err := v.set(cfg, val); err != nil {
			return nil, err
		}
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

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
