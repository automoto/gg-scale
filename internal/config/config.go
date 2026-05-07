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
	HTTPAddr string
	// GameServerPublicIP is the public IP or hostname returned to game clients
	// so they can connect directly to a game server container. Required when
	// FLEET_BACKEND=docker or FLEET_BACKEND=static (Phase 2). Empty is fine
	// for local dev.
	GameServerPublicIP string
	DatabaseURL        string
	LogLevel           string
	Env                string
	JWTSigningKey      string

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
	// fileFallback enables the <name>_FILE convention: if <name>_FILE is set
	// in the environment, its content is read from that path (trimmed of
	// trailing whitespace) and used in place of <name>. Lets operators mount
	// docker/k8s/Vault file-based secrets without exposing them in env vars.
	fileFallback bool
	set          func(*Config, string) error
}

var declared = []varDecl{
	{name: "DATABASE_URL", required: true, fileFallback: true, set: func(c *Config, v string) error { c.DatabaseURL = v; return nil }},
	{name: "HTTP_ADDR", defval: ":8080", set: func(c *Config, v string) error { c.HTTPAddr = v; return nil }},
	{name: "GAME_SERVER_PUBLIC_IP", defval: "", set: func(c *Config, v string) error {
		c.GameServerPublicIP = v
		return nil
	}},
	{name: "LOG_LEVEL", defval: "info", set: func(c *Config, v string) error { c.LogLevel = v; return nil }},
	{name: "ENV", defval: "dev", set: func(c *Config, v string) error { c.Env = v; return nil }},
	{name: "JWT_SIGNING_KEY", fileFallback: true, set: func(c *Config, v string) error { c.JWTSigningKey = v; return nil }},
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
		val, err := resolveValue(v)
		if err != nil {
			return nil, err
		}
		if val == "" && v.required {
			return nil, fmt.Errorf("required env var %s is missing", v.name)
		}
		if val == "" {
			val = v.defval
		}
		if err := v.set(cfg, val); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// resolveValue looks up the env value for a varDecl, honoring the
// <name>_FILE convention when the decl opts in. _FILE wins over the plain
// env var; reading the file is required to succeed if the path is set.
func resolveValue(v varDecl) (string, error) {
	if !v.fileFallback {
		return os.Getenv(v.name), nil
	}
	path := os.Getenv(v.name + "_FILE")
	if path == "" {
		return os.Getenv(v.name), nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied secret path is the documented contract
	if err != nil {
		return "", fmt.Errorf("read %s_FILE %q: %w", v.name, path, err)
	}
	return strings.TrimRight(string(data), " \t\r\n"), nil
}

// DeclaredVars returns the list of env-var names this package reads,
// including <name>_FILE variants for vars that support file fallback.
// Used by the drift test to compare against .env.example.
func DeclaredVars() []string {
	out := make([]string, 0, len(declared))
	for _, v := range declared {
		out = append(out, v.name)
		if v.fileFallback {
			out = append(out, v.name+"_FILE")
		}
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
