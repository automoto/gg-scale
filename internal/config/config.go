// Package config loads runtime configuration from environment variables.
// All declared vars must also appear in .env.example; the drift test in
// config_test.go enforces this contract.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds runtime configuration loaded from the environment.
type Config struct {
	HTTPAddr string
	// GameServerPublicIP is the public IP or hostname returned to game clients
	// so they can connect directly to a game server container. Required when
	// FLEET_BACKEND=docker. Empty is fine for local dev.
	GameServerPublicIP string

	// FleetBackend selects the fleet allocator: docker | agones | openstack |
	// plugin:<name>. Default "docker" matches the single-VPS self-host story.
	FleetBackend string
	// FleetRegion is the region label persisted on every allocation and used
	// by Agones GameServerSelector / region-aware backends. Default "local".
	FleetRegion string
	// FleetPluginDir is the directory scanned for ggscale-fleet-* plugin
	// binaries. Default "/etc/ggscale/plugins". Only consulted when
	// FleetBackend starts with "plugin:".
	FleetPluginDir string

	// Docker fleet-backend tunables. Only consulted when FleetBackend=docker.
	DockerGameServerImage string
	DockerGameServerPort  int
	DockerProbeType       string
	DockerProbePath       string
	DockerMaxSessions     int
	DockerHost            string

	// Agones fleet-backend tunables. Only consulted when FleetBackend=agones.
	AgonesNamespace      string
	AgonesFleetName      string
	AgonesSelectorLabels string
	AgonesKubeconfig     string

	// RealtimeMaxPerTenant caps concurrent /v1/ws connections per tenant.
	// 0 disables the cap (the cache.Store layer is bypassed entirely).
	RealtimeMaxPerTenant int64

	// MatchmakerBucketSize is the number of queued tickets that must
	// accumulate in a (tenant, project, region, game_mode) bucket before
	// the worker calls fleet.Manager.Allocate. Default 1.
	MatchmakerBucketSize int
	// MatchmakerInterval is the fallback scan cadence for the worker.
	// The hot path is Postgres LISTEN/NOTIFY, which wakes the worker in
	// milliseconds; this ticker only catches tickets queued during a
	// listener reconnect gap. Default 5s.
	MatchmakerInterval time.Duration

	// TURN relay tunables. The relay is disabled unless RelayPublicIP and
	// RelaySharedSecret are both set.
	RelayPublicIP     string
	RelayBindAddr     string
	RelayUDPPort      int
	RelayRealm        string
	RelaySharedSecret string
	RelayCredTTL      time.Duration
	DatabaseURL       string
	LogLevel          string
	Env               string
	JWTSigningKey     string

	// DashboardEnabled controls whether /v1/dashboard is mounted.
	DashboardEnabled bool
	// DashboardBootstrapTokenFile optionally writes the first-run token to a file.
	DashboardBootstrapTokenFile string
	// DashboardCookieSecure sets the Secure flag on the dashboard session cookie.
	DashboardCookieSecure bool
	// DashboardBaseURL is the externally-visible origin prefixed onto magic
	// links emitted in dashboard invite emails. Empty means relative paths
	// (fine for local dev; bad for production).
	DashboardBaseURL string
	// PlayersEnabled mounts /v1/players for player-facing signup/verify/login.
	PlayersEnabled bool

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
	{name: "DASHBOARD_BASE_URL", defval: "", set: func(c *Config, v string) error {
		c.DashboardBaseURL = v
		return nil
	}},
	{name: "PLAYERS_ENABLED", defval: "true", set: func(c *Config, v string) error {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("PLAYERS_ENABLED %q: %w", v, err)
		}
		c.PlayersEnabled = enabled
		return nil
	}},

	{name: "FLEET_BACKEND", defval: "docker", set: func(c *Config, v string) error {
		c.FleetBackend = v
		return nil
	}},
	{name: "FLEET_REGION", defval: "local", set: func(c *Config, v string) error {
		c.FleetRegion = v
		return nil
	}},
	{name: "FLEET_PLUGIN_DIR", defval: "/etc/ggscale/plugins", set: func(c *Config, v string) error {
		c.FleetPluginDir = v
		return nil
	}},

	{name: "DOCKER_GAMESERVER_IMAGE", defval: "", set: func(c *Config, v string) error {
		c.DockerGameServerImage = v
		return nil
	}},
	{name: "DOCKER_GAMESERVER_PORT", defval: "7777", set: func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("DOCKER_GAMESERVER_PORT %q: %w", v, err)
		}
		c.DockerGameServerPort = n
		return nil
	}},
	{name: "DOCKER_PROBE_TYPE", defval: "tcp", set: func(c *Config, v string) error {
		switch v {
		case "", "tcp", "http":
			c.DockerProbeType = v
			return nil
		default:
			return fmt.Errorf("DOCKER_PROBE_TYPE %q: must be tcp|http (empty disables)", v)
		}
	}},
	{name: "DOCKER_PROBE_PATH", defval: "/healthz", set: func(c *Config, v string) error {
		c.DockerProbePath = v
		return nil
	}},
	{name: "DOCKER_MAX_SESSIONS", defval: "0", set: func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("DOCKER_MAX_SESSIONS %q: %w", v, err)
		}
		c.DockerMaxSessions = n
		return nil
	}},
	{name: "DOCKER_HOST", defval: "", set: func(c *Config, v string) error {
		c.DockerHost = v
		return nil
	}},

	{name: "AGONES_NAMESPACE", defval: "default", set: func(c *Config, v string) error {
		c.AgonesNamespace = v
		return nil
	}},
	{name: "AGONES_FLEET_NAME", defval: "", set: func(c *Config, v string) error {
		c.AgonesFleetName = v
		return nil
	}},
	{name: "AGONES_SELECTOR_LABELS", defval: "", set: func(c *Config, v string) error {
		c.AgonesSelectorLabels = v
		return nil
	}},
	{name: "AGONES_KUBECONFIG", defval: "", set: func(c *Config, v string) error {
		c.AgonesKubeconfig = v
		return nil
	}},

	{name: "REALTIME_MAX_PER_TENANT", defval: "0", set: func(c *Config, v string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("REALTIME_MAX_PER_TENANT %q: must be a non-negative integer", v)
		}
		c.RealtimeMaxPerTenant = n
		return nil
	}},

	{name: "MATCHMAKER_BUCKET_SIZE", defval: "1", set: func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("MATCHMAKER_BUCKET_SIZE %q: must be a positive integer", v)
		}
		c.MatchmakerBucketSize = n
		return nil
	}},
	{name: "MATCHMAKER_INTERVAL", defval: "5s", set: func(c *Config, v string) error {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("MATCHMAKER_INTERVAL %q: must be a positive duration", v)
		}
		c.MatchmakerInterval = d
		return nil
	}},

	{name: "RELAY_PUBLIC_IP", defval: "", set: func(c *Config, v string) error {
		c.RelayPublicIP = v
		return nil
	}},
	{name: "RELAY_BIND_ADDR", defval: "0.0.0.0", set: func(c *Config, v string) error {
		c.RelayBindAddr = v
		return nil
	}},
	{name: "RELAY_UDP_PORT", defval: "3478", set: func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 65535 {
			return fmt.Errorf("RELAY_UDP_PORT %q: must be 1..65535", v)
		}
		c.RelayUDPPort = n
		return nil
	}},
	{name: "RELAY_REALM", defval: "ggscale", set: func(c *Config, v string) error {
		c.RelayRealm = v
		return nil
	}},
	{name: "RELAY_SHARED_SECRET", fileFallback: true, set: func(c *Config, v string) error {
		c.RelaySharedSecret = v
		return nil
	}},
	{name: "RELAY_CRED_TTL", defval: "5m", set: func(c *Config, v string) error {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("RELAY_CRED_TTL %q: must be a positive duration", v)
		}
		c.RelayCredTTL = d
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
