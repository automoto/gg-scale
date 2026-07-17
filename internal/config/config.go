// Package config loads runtime configuration from environment variables.
// All declared vars must also appear in .env.example; the drift test in
// config_test.go enforces this contract.
//
// The Config struct is the single declaration point: each field carries an
// `env:"NAME"` tag (plus `envDefault:"..."` where a default applies) parsed by
// github.com/caarlos0/env. The `envFile:"true"` tag opts a secret field into
// the <NAME>_FILE convention handled in load.go. Cross-field invariants live in
// validate.go.
package config

import "time"

// Config holds runtime configuration loaded from the environment.
type Config struct {
	HTTPAddr string `env:"HTTP_ADDR" envDefault:":8080"`
	// AppRegion separates regional realtime connection-cap grants. All app
	// instances in one load-balancer pool use the same value; different regions
	// must use different values.
	AppRegion string `env:"APP_REGION" envDefault:"local"`

	// HTTPRequestTimeout bounds how long a non-streaming request may run before
	// the middleware cancels its context and returns 503 + Retry-After. It must
	// stay below the server WriteTimeout so a saturated pool fails fast instead
	// of queuing acquires until the connection is force-closed. WebSocket and
	// other hijacked paths are exempt (they manage their own deadlines).
	HTTPRequestTimeout time.Duration `env:"HTTP_REQUEST_TIMEOUT" envDefault:"15s"`

	// MetricsAuthToken, when set, gates the /metrics endpoint behind a bearer
	// token (Authorization: Bearer <token>); empty leaves /metrics open.
	// Supports the _FILE convention. config.Validate requires it in production
	// unless MetricsAuthDisabled is set.
	MetricsAuthToken string `env:"METRICS_AUTH_TOKEN" envFile:"true"`
	// MetricsAuthDisabled explicitly serves /metrics unauthenticated in
	// production; without it, an empty MetricsAuthToken refuses to boot.
	MetricsAuthDisabled bool `env:"METRICS_AUTH_DISABLED" envDefault:"false"`
	// GameServerPublicIP is the public IP or hostname returned to game clients
	// so they can connect directly to a game server container. Required when
	// FLEET_BACKEND=docker. Empty is fine for local dev.
	GameServerPublicIP string `env:"GAME_SERVER_PUBLIC_IP"`

	// FeatureFleetEnabled is the startup kill switch for dedicated game-server
	// fleets. Defaults false: no fleet manager is built and every fleet entry
	// point refuses regardless of FleetBackend. Must be true for FleetBackend
	// to take effect.
	FeatureFleetEnabled bool `env:"FEATURE_FLEET_ENABLED" envDefault:"false"`
	// FeatureP2PRelayEnabled is the startup kill switch for the TURN/P2P relay.
	// Defaults false: no relay issuer or UDP listener is built and relay entry
	// points refuse regardless of RelaySharedSecret.
	FeatureP2PRelayEnabled bool `env:"FEATURE_P2P_RELAY_ENABLED" envDefault:"false"`

	// FleetBackend selects the fleet allocator: docker | agones | openstack |
	// plugin:<name>. Empty by default so fleets stay off until an operator
	// opts in; only consulted when FeatureFleetEnabled is true.
	FleetBackend string `env:"FLEET_BACKEND"`
	// FleetRegion is the region label persisted on every allocation and used
	// by Agones GameServerSelector / region-aware backends. Default "local".
	FleetRegion string `env:"FLEET_REGION" envDefault:"local"`
	// FleetPluginDir is the directory scanned for ggscale-fleet-* plugin
	// binaries. Default "/etc/ggscale/plugins". Only consulted when
	// FleetBackend starts with "plugin:".
	FleetPluginDir string `env:"FLEET_PLUGIN_DIR" envDefault:"/etc/ggscale/plugins"`

	// Docker fleet-backend host-wide tunables. Per-template values (image,
	// port, probe) live on the fleet template, not in env vars.
	DockerHost string `env:"DOCKER_HOST"`

	// Agones fleet-backend host-wide tunables. Per-template values (fleet
	// name, selector labels) live on the fleet template, not in env vars.
	AgonesNamespace  string `env:"AGONES_NAMESPACE" envDefault:"default"`
	AgonesKubeconfig string `env:"AGONES_KUBECONFIG"`

	// k3s API auth via ServiceAccount + bearer token, for deployments where
	// ggscale-server runs outside the cluster (e.g. as a Dokku app on a
	// separate host) and can't use in-cluster config or ship a kubeconfig
	// file. When all three are set, they take precedence over
	// AgonesKubeconfig. K3sCACertB64 is base64-encoded PEM.
	K3sAPIURL    string `env:"K3S_API_URL"`
	K3sSAToken   string `env:"K3S_SA_TOKEN" envFile:"true"`
	K3sCACertB64 string `env:"K3S_CA_CERT_B64" envFile:"true"`

	// RealtimeMaxPerTenant overrides the per-tenant /v1/ws connection cap with
	// a fixed hard limit (no burst). 0 (default) uses the tenant's tier-class
	// envelope from ConnectionCapForClass; set a value only to pin a single
	// fixed cap for all tenants (self-host escape hatch).
	RealtimeMaxPerTenant int64 `env:"REALTIME_MAX_PER_TENANT" envDefault:"0"`
	// RealtimeMaxPerPlayer caps concurrent /v1/ws connections from a
	// single player so one misbehaving player can't drain the per-tenant
	// budget. 0 disables. Default 4.
	RealtimeMaxPerPlayer int64 `env:"REALTIME_MAX_PER_PLAYER" envDefault:"4"`

	// MatchmakerRelaxAfter is how long the oldest member of a below-max
	// group waits before the group commits at a smaller (still valid)
	// size. Default 30s.
	MatchmakerRelaxAfter time.Duration `env:"MATCHMAKER_RELAX_AFTER" envDefault:"30s"`
	// MatchmakerRegionRelaxAfter is how long a bucket's oldest
	// widen-eligible ticket waits before cross-region grouping unlocks.
	// 0 disables widening. Default 60s.
	MatchmakerRegionRelaxAfter time.Duration `env:"MATCHMAKER_REGION_RELAX_AFTER" envDefault:"60s"`
	// MatchmakerInterval is the fallback scan cadence for the worker.
	// The hot path is Postgres LISTEN/NOTIFY, which wakes the worker in
	// milliseconds; this ticker only catches tickets queued during a
	// listener reconnect gap. Default 5s.
	MatchmakerInterval time.Duration `env:"MATCHMAKER_INTERVAL" envDefault:"5s"`
	// MatchmakerClaimTTL bounds how long a worker holds a claim before the
	// sweeper reclaims it. Should be larger than the slowest expected
	// Allocate latency. Default 60s.
	MatchmakerClaimTTL time.Duration `env:"MATCHMAKER_CLAIM_TTL" envDefault:"60s"`
	// MatchmakerMaxAttempts is how many allocate-failed releases a ticket
	// survives before flipping to 'failed'. Default 3.
	MatchmakerMaxAttempts int `env:"MATCHMAKER_MAX_ATTEMPTS" envDefault:"3"`
	// MatchmakerWorkerCount is the size of the bucket-processing fan-out
	// pool. Default 4. Higher lets slow backends run in parallel without
	// back-pressuring the LISTEN reader.
	MatchmakerWorkerCount int `env:"MATCHMAKER_WORKER_COUNT" envDefault:"4"`
	// MatchmakerSweepInterval is how often the cleanup goroutine releases
	// claims whose lease has expired. Default 60s.
	MatchmakerSweepInterval time.Duration `env:"MATCHMAKER_SWEEP_INTERVAL" envDefault:"60s"`
	// MatchmakerMaxTicketsPerPlayer caps a player's concurrently queued
	// tickets per project. 0 disables the cap. Default 3.
	MatchmakerMaxTicketsPerPlayer int `env:"MATCHMAKER_MAX_TICKETS_PER_PLAYER" envDefault:"3"`
	// MatchmakerTicketTTL is how long a queued ticket lives before the
	// sweeper fails it. 0 disables expiry. Default 10m.
	MatchmakerTicketTTL time.Duration `env:"MATCHMAKER_TICKET_TTL" envDefault:"10m"`

	// TURN relay tunables. The relay is disabled unless RelayPublicIP and
	// RelaySharedSecret are both set.
	RelayPublicIP     string        `env:"RELAY_PUBLIC_IP"`
	RelayBindAddr     string        `env:"RELAY_BIND_ADDR" envDefault:"0.0.0.0"`
	RelayUDPPort      int           `env:"RELAY_UDP_PORT" envDefault:"3478"`
	RelayRealm        string        `env:"RELAY_REALM" envDefault:"ggscale"`
	RelaySharedSecret string        `env:"RELAY_SHARED_SECRET" envFile:"true"`
	RelayCredTTL      time.Duration `env:"RELAY_CRED_TTL" envDefault:"5m"`
	DatabaseURL       string        `env:"DATABASE_URL,required" envFile:"true"`
	// DBMigrateURL is an elevated DSN used only to apply schema
	// migrations at startup (DDL, CREATE ROLE, FORCE RLS, CREATE POLICY).
	// It is required in production; outside production, empty falls back to
	// DATABASE_URL for zero-config self-hosting. Setting it lets DATABASE_URL
	// be a least-privilege login role (member of ggscale_app, no superuser/DDL)
	// while migrations run as the owner. Supports the _FILE convention.
	DBMigrateURL  string `env:"DB_MIGRATE_URL" envFile:"true"`
	LogLevel      string `env:"LOG_LEVEL" envDefault:"info"`
	Env           string `env:"ENV" envDefault:"dev"`
	JWTSigningKey string `env:"JWT_SIGNING_KEY" envFile:"true"`
	// EmailVerifySigningKey is the independent 32-byte HMAC key shared by
	// player and control-panel verification cookies. It is required in
	// production; elsewhere an empty value is generated into server_secrets.
	EmailVerifySigningKey string `env:"EMAIL_VERIFY_SIGNING_KEY" envFile:"true"`
	// TwoFactorEncKey is an optional 32-byte hex AES key that encrypts TOTP
	// secrets at rest and derives the 2FA pending-cookie HMAC key. When empty
	// the server auto-generates and persists a key (zero-config self-host); set
	// this to pin key material explicitly. Changing or removing it after users
	// have enrolled locks those logins out (fail closed) until the prior key is
	// restored as a decrypt fallback.
	TwoFactorEncKey string `env:"TWO_FACTOR_ENC_KEY" envFile:"true"`
	// MigrationsDir is the directory ggscale-server reads SQL migrations
	// from on startup. Default `/migrations` matches the Dockerfile COPY.
	// In local dev (compose, go run) override with the repo-relative path.
	MigrationsDir string `env:"MIGRATIONS_DIR" envDefault:"/migrations"`

	// ControlPanelDisabled is the raw CONTROL_PANEL_DISABLED switch;
	// ControlPanelEnabled is its negation, derived in Load.
	ControlPanelDisabled bool `env:"CONTROL_PANEL_DISABLED" envDefault:"false"`
	// ControlPanelEnabled controls whether /v1/control-panel is mounted.
	ControlPanelEnabled bool `env:"-"`
	// ControlPanelBootstrapTokenFile optionally writes the first-run token to a file.
	ControlPanelBootstrapTokenFile string `env:"CONTROL_PANEL_BOOTSTRAP_TOKEN_FILE"`
	// ControlPanelCookieSecure sets the Secure flag on the control panel session cookie.
	ControlPanelCookieSecure bool `env:"CONTROL_PANEL_COOKIE_SECURE" envDefault:"false"`
	// ControlPanelBaseURL is the externally-visible origin prefixed onto magic
	// links emitted in control panel invite emails. Empty means relative paths
	// (fine for local dev; bad for production).
	ControlPanelBaseURL string `env:"CONTROL_PANEL_BASE_URL"`
	// PlayersEnabled mounts /v1/players for player-facing signup/verify/login.
	PlayersEnabled bool `env:"PLAYERS_ENABLED" envDefault:"true"`

	// CORSAllowedOrigins is the comma-separated list of origins permitted by
	// the API router. Empty in dev allows "*"; in production an empty list
	// is rejected by Validate.
	CORSAllowedOrigins []string `env:"CORS_ALLOWED_ORIGINS"`

	// DBMaxConns / DBMinConns size the pgx pool. Defaults: 25 / 2. The
	// LISTEN connection holds one slot for the process lifetime, so
	// effective request concurrency is DBMaxConns - 1 - matchmaker workers.
	DBMaxConns        int           `env:"DB_MAX_CONNS" envDefault:"25"`
	DBMinConns        int           `env:"DB_MIN_CONNS" envDefault:"2"`
	DBMaxConnLifetime time.Duration `env:"DB_MAX_CONN_LIFETIME" envDefault:"1h"`
	// DBMaxConnIdleTime returns idle connections after this long. A traffic
	// spike grows the pool to DBMaxConns; without an idle cap those
	// connections are held until DBMaxConnLifetime (1h). Trimming them frees
	// pool slots on the shared write master within minutes of the spike.
	DBMaxConnIdleTime time.Duration `env:"DB_MAX_CONN_IDLE_TIME" envDefault:"10m"`
	// DBStatementTimeout bounds runaway queries. Set via SET LOCAL in Q().
	DBStatementTimeout time.Duration `env:"DB_STATEMENT_TIMEOUT" envDefault:"30s"`

	// DBReadURL, when set, points at a read replica for staleness-tolerant
	// reads (leaderboard/friends/presence/storage GETs). Empty means the read
	// pool aliases the primary, so every host runs identical code and only the
	// hosts near a replica (e.g. west) set this. Supports the _FILE convention.
	DBReadURL string `env:"DB_READ_URL" envFile:"true"`
	// DBReadMaxConns sizes the read pool. Only consulted when DBReadURL is set.
	DBReadMaxConns int `env:"DB_READ_MAX_CONNS" envDefault:"25"`

	// StorageMaxValueBytes is the platform default cap on a single storage
	// object's value. Per-tenant / per-project overrides (storage_limits) may
	// raise or lower it; the effective limit is resolved per write.
	StorageMaxValueBytes int64 `env:"STORAGE_MAX_VALUE_BYTES" envDefault:"1048576"`

	// QuotasEnforceNewTenants makes tenant provisioning (control-panel create
	// and signup-request acceptance) set enforce_quotas=true on the new tenant.
	// Default false keeps zero-config self-host uncapped; the managed prod
	// deploy sets it so all new tenants are enforced. Existing tenants are
	// unaffected — flip them with a one-time UPDATE (operator runbook).
	QuotasEnforceNewTenants bool `env:"QUOTAS_ENFORCE_NEW_TENANTS" envDefault:"false"`

	// SMTPTLS selects how the mailer establishes TLS: "off", "starttls"
	// (default; hard-fails if the server doesn't advertise it), or
	// "implicit" (TLS from connect, typically port 465).
	SMTPTLS string `env:"SMTP_TLS" envDefault:"starttls"`

	// DockerBindIP is the host interface the docker fleet backend binds
	// container ports to. Default "127.0.0.1"; set to a public IP for
	// production multi-host setups.
	DockerBindIP string `env:"DOCKER_BIND_IP" envDefault:"127.0.0.1"`
	// DockerDefaultMemory / CPUs / Pids are the per-container resource
	// caps applied when a fleet template doesn't specify its own.
	DockerDefaultMemory int64   `env:"DOCKER_DEFAULT_MEMORY" envDefault:"536870912"`
	DockerDefaultCPUs   float64 `env:"DOCKER_DEFAULT_CPUS" envDefault:"1.0"`
	DockerDefaultPids   int64   `env:"DOCKER_DEFAULT_PIDS" envDefault:"256"`
	// DockerRegistryAllowlist restricts which image registries may run.
	// Empty disables the check.
	DockerRegistryAllowlist []string `env:"DOCKER_REGISTRY_ALLOWLIST"`
	// DockerRequireDigest forces every image to carry an @sha256:… pin.
	// Required in production when FleetBackend=docker.
	DockerRequireDigest bool `env:"DOCKER_REQUIRE_DIGEST" envDefault:"false"`

	// TrustedProxyHeader names a request header (e.g. "CF-Connecting-IP")
	// to honor when RemoteAddr is in a trusted-proxy network. Empty
	// disables forwarded-IP trust.
	TrustedProxyHeader string `env:"TRUSTED_PROXY_HEADER"`
	// TrustedProxyCIDRs is the allowlist of TCP peer networks permitted to
	// supply TrustedProxyHeader.
	TrustedProxyCIDRs []string `env:"TRUSTED_PROXY_CIDRS"`

	// Mail provider and connection settings.
	// MailProvider selects the registered provider: "smtp" (default) or "noop".
	// External providers register via mailer.Register in their init().
	MailProvider string `env:"MAIL_PROVIDER" envDefault:"smtp"`
	// SMTPAddr is the SMTP server address (host:port). Default "localhost:1025"
	// matches MailHog in the dev compose stack.
	SMTPAddr string `env:"SMTP_ADDR" envDefault:"localhost:1025"`
	// SMTPUser is the SMTP username. Leave empty for unauthenticated relays
	// (e.g. MailHog in dev).
	SMTPUser string `env:"SMTP_USER"`
	// SMTPPassword is the SMTP password. Unused when SMTPUser is empty.
	SMTPPassword string `env:"SMTP_PASSWORD"`
	// MailFrom is the From address on outbound mail. Default "noreply@ggscale.dev".
	MailFrom string `env:"MAIL_FROM" envDefault:"noreply@ggscale.dev"`
}
