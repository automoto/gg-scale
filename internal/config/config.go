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

	// Docker fleet-backend host-wide tunables. Per-template values (image,
	// port, probe) live on the fleet template, not in env vars.
	DockerHost string

	// Agones fleet-backend host-wide tunables. Per-template values (fleet
	// name, selector labels) live on the fleet template, not in env vars.
	AgonesNamespace  string
	AgonesKubeconfig string

	// RealtimeMaxPerTenant caps concurrent /v1/ws connections per tenant.
	// 0 disables the cap (the cache.Store layer is bypassed entirely).
	RealtimeMaxPerTenant int64
	// RealtimeMaxPerEndUser caps concurrent /v1/ws connections from a
	// single player so one misbehaving end-user can't drain the per-tenant
	// budget. 0 disables. Default 4.
	RealtimeMaxPerEndUser int64

	// MatchmakerBucketSize is the number of queued tickets that must
	// accumulate in a (tenant, project, region, game_mode) bucket before
	// the worker calls fleet.Manager.Allocate. Default 1.
	MatchmakerBucketSize int
	// MatchmakerInterval is the fallback scan cadence for the worker.
	// The hot path is Postgres LISTEN/NOTIFY, which wakes the worker in
	// milliseconds; this ticker only catches tickets queued during a
	// listener reconnect gap. Default 5s.
	MatchmakerInterval time.Duration
	// MatchmakerClaimTTL bounds how long a worker holds a claim before the
	// sweeper reclaims it. Should be larger than the slowest expected
	// Allocate latency. Default 60s.
	MatchmakerClaimTTL time.Duration
	// MatchmakerMaxAttempts is how many allocate-failed releases a ticket
	// survives before flipping to 'failed'. Default 3.
	MatchmakerMaxAttempts int
	// MatchmakerWorkerCount is the size of the bucket-processing fan-out
	// pool. Default 4. Higher lets slow backends run in parallel without
	// back-pressuring the LISTEN reader.
	MatchmakerWorkerCount int
	// MatchmakerSweepInterval is how often the cleanup goroutine releases
	// claims whose lease has expired. Default 60s.
	MatchmakerSweepInterval time.Duration

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

	// CORSAllowedOrigins is the comma-separated list of origins permitted by
	// the API router. Empty in dev allows "*"; in production an empty list
	// is rejected by Validate.
	CORSAllowedOrigins []string

	// DBMaxConns / DBMinConns size the pgx pool. Defaults: 25 / 2. The
	// LISTEN connection holds one slot for the process lifetime, so
	// effective request concurrency is DBMaxConns - 1 - matchmaker workers.
	DBMaxConns        int
	DBMinConns        int
	DBMaxConnLifetime time.Duration
	// DBStatementTimeout bounds runaway queries. Set via SET LOCAL in Q().
	DBStatementTimeout time.Duration

	// SMTPTLS selects how the mailer establishes TLS: "off", "starttls"
	// (default; hard-fails if the server doesn't advertise it), or
	// "implicit" (TLS from connect, typically port 465).
	SMTPTLS string

	// DockerBindIP is the host interface the docker fleet backend binds
	// container ports to. Default "127.0.0.1"; set to a public IP for
	// production multi-host setups.
	DockerBindIP string
	// DockerDefaultMemory / CPUs / Pids are the per-container resource
	// caps applied when a fleet template doesn't specify its own.
	DockerDefaultMemory int64
	DockerDefaultCPUs   float64
	DockerDefaultPids   int64
	// DockerRegistryAllowlist restricts which image registries may run.
	// Empty disables the check.
	DockerRegistryAllowlist []string
	// DockerRequireDigest forces every image to carry an @sha256:… pin.
	// Required in production when FleetBackend=docker.
	DockerRequireDigest bool

	// TrustedProxyHeader names a request header (e.g. "CF-Connecting-IP")
	// to honor when RemoteAddr is in a trusted-proxy network. Empty
	// disables forwarded-IP trust.
	TrustedProxyHeader string

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

	{name: "DOCKER_HOST", defval: "", set: func(c *Config, v string) error {
		c.DockerHost = v
		return nil
	}},

	{name: "AGONES_NAMESPACE", defval: "default", set: func(c *Config, v string) error {
		c.AgonesNamespace = v
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
	{name: "REALTIME_MAX_PER_END_USER", defval: "4", set: func(c *Config, v string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("REALTIME_MAX_PER_END_USER %q: must be a non-negative integer", v)
		}
		c.RealtimeMaxPerEndUser = n
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
	{name: "MATCHMAKER_CLAIM_TTL", defval: "60s", set: func(c *Config, v string) error {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("MATCHMAKER_CLAIM_TTL %q: must be a positive duration", v)
		}
		c.MatchmakerClaimTTL = d
		return nil
	}},
	{name: "MATCHMAKER_MAX_ATTEMPTS", defval: "3", set: func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("MATCHMAKER_MAX_ATTEMPTS %q: must be a positive integer", v)
		}
		c.MatchmakerMaxAttempts = n
		return nil
	}},
	{name: "MATCHMAKER_WORKER_COUNT", defval: "4", set: func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("MATCHMAKER_WORKER_COUNT %q: must be a positive integer", v)
		}
		c.MatchmakerWorkerCount = n
		return nil
	}},
	{name: "MATCHMAKER_SWEEP_INTERVAL", defval: "60s", set: func(c *Config, v string) error {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("MATCHMAKER_SWEEP_INTERVAL %q: must be a positive duration", v)
		}
		c.MatchmakerSweepInterval = d
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

	{name: "CORS_ALLOWED_ORIGINS", defval: "", set: func(c *Config, v string) error {
		c.CORSAllowedOrigins = splitCSV(v)
		return nil
	}},
	{name: "SMTP_TLS", defval: "starttls", set: func(c *Config, v string) error {
		switch v {
		case "off", "starttls", "implicit":
			c.SMTPTLS = v
			return nil
		default:
			return fmt.Errorf("SMTP_TLS %q: must be one of off|starttls|implicit", v)
		}
	}},
	{name: "DB_MAX_CONNS", defval: "25", set: func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("DB_MAX_CONNS %q: must be a positive integer", v)
		}
		c.DBMaxConns = n
		return nil
	}},
	{name: "DB_MIN_CONNS", defval: "2", set: func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("DB_MIN_CONNS %q: must be a non-negative integer", v)
		}
		c.DBMinConns = n
		return nil
	}},
	{name: "DB_MAX_CONN_LIFETIME", defval: "1h", set: func(c *Config, v string) error {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("DB_MAX_CONN_LIFETIME %q: must be a positive duration", v)
		}
		c.DBMaxConnLifetime = d
		return nil
	}},
	{name: "DB_STATEMENT_TIMEOUT", defval: "30s", set: func(c *Config, v string) error {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("DB_STATEMENT_TIMEOUT %q: must be a positive duration", v)
		}
		c.DBStatementTimeout = d
		return nil
	}},
	{name: "DOCKER_BIND_IP", defval: "127.0.0.1", set: func(c *Config, v string) error {
		c.DockerBindIP = v
		return nil
	}},
	{name: "DOCKER_DEFAULT_MEMORY", defval: "536870912", set: func(c *Config, v string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("DOCKER_DEFAULT_MEMORY %q: must be a non-negative integer", v)
		}
		c.DockerDefaultMemory = n
		return nil
	}},
	{name: "DOCKER_DEFAULT_CPUS", defval: "1.0", set: func(c *Config, v string) error {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return fmt.Errorf("DOCKER_DEFAULT_CPUS %q: must be a non-negative number", v)
		}
		c.DockerDefaultCPUs = f
		return nil
	}},
	{name: "DOCKER_DEFAULT_PIDS", defval: "256", set: func(c *Config, v string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("DOCKER_DEFAULT_PIDS %q: must be a non-negative integer", v)
		}
		c.DockerDefaultPids = n
		return nil
	}},
	{name: "DOCKER_REGISTRY_ALLOWLIST", defval: "", set: func(c *Config, v string) error {
		c.DockerRegistryAllowlist = splitCSV(v)
		return nil
	}},
	{name: "DOCKER_REQUIRE_DIGEST", defval: "false", set: func(c *Config, v string) error {
		switch strings.ToLower(v) {
		case "true", "1", "yes":
			c.DockerRequireDigest = true
		case "false", "0", "no", "":
			c.DockerRequireDigest = false
		default:
			return fmt.Errorf("DOCKER_REQUIRE_DIGEST %q: must be true|false", v)
		}
		return nil
	}},
	{name: "TRUSTED_PROXY_HEADER", defval: "", set: func(c *Config, v string) error {
		c.TrustedProxyHeader = v
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
	if err := cfg.Validate(); err != nil {
		return nil, err
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
