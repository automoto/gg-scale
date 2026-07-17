package config

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"
)

func isLoopbackOrLinkLocal(s string) bool {
	if s == "" {
		return false
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

// IsProduction reports whether the deployment environment is production, accepting
// both "production" and the "prod" shorthand, case-insensitively.
func (c *Config) IsProduction() bool {
	return strings.EqualFold(c.Env, "production") || strings.EqualFold(c.Env, "prod")
}

// Validate runs cross-field checks that no single var-decl can express.
// Called by Load after every individual setter has run. Returning an
// error blocks startup so a misconfigured production deployment fails
// loud at boot rather than silently weakening its own posture.
//
// All checks here are defence-in-depth: any single failure means the
// operator has explicitly opted into an unsafe configuration, and we
// would rather refuse to run than serve traffic from one. Each check is a
// focused method; the first failure wins.
func (c *Config) Validate() error {
	checks := []func() error{
		c.checkSecretLengths,
		c.checkFeatureSwitches,
		c.checkDBPool,
		c.checkMailProvider,
		c.checkProductionPosture,
		c.checkAgonesAuth,
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	c.warnLowDBPool()
	c.warnRequestTimeout()
	return nil
}

// serverWriteTimeout mirrors the http.Server WriteTimeout in
// cmd/ggscale-server/main.go. A request deadline at or above it never fires
// before the connection is force-closed, defeating the fast-fail behavior.
const serverWriteTimeout = 30 * time.Second

// warnRequestTimeout logs a non-fatal hint when the request deadline is not
// comfortably below the server WriteTimeout.
func (c *Config) warnRequestTimeout() {
	if c.HTTPRequestTimeout >= serverWriteTimeout {
		slog.Warn("HTTP_REQUEST_TIMEOUT is not below the server WriteTimeout; the deadline may never fire",
			"value", c.HTTPRequestTimeout, "write_timeout", serverWriteTimeout)
	}
}

// checkSecretLengths rejects shared secrets short enough to brute-force.
func (c *Config) checkSecretLengths() error {
	if c.RelaySharedSecret != "" && len(c.RelaySharedSecret) < 32 {
		return fmt.Errorf("RELAY_SHARED_SECRET must be >= 32 bytes when set (got %d)", len(c.RelaySharedSecret))
	}
	if c.MetricsAuthToken != "" && len(c.MetricsAuthToken) < 32 {
		return fmt.Errorf("METRICS_AUTH_TOKEN must be >= 32 bytes when set (got %d)", len(c.MetricsAuthToken))
	}
	return nil
}

// checkFeatureSwitches refuses configs that wire up a feature whose kill
// switch is off, rather than silently leaving the feature dark.
func (c *Config) checkFeatureSwitches() error {
	if !c.FeatureP2PRelayEnabled && (c.RelayPublicIP != "" || c.RelaySharedSecret != "") {
		return fmt.Errorf("RELAY_PUBLIC_IP/RELAY_SHARED_SECRET set while FEATURE_P2P_RELAY_ENABLED is false; enable the feature or clear the relay config")
	}
	if !c.FeatureFleetEnabled && c.FleetBackend != "" {
		return fmt.Errorf("FLEET_BACKEND=%q set while FEATURE_FLEET_ENABLED is false; enable the feature or unset FLEET_BACKEND", c.FleetBackend)
	}
	return nil
}

// checkDBPool enforces the pgx pool sizing invariants.
func (c *Config) checkDBPool() error {
	if c.DBMaxConns < 4 {
		return fmt.Errorf("DB_MAX_CONNS must be >= 4 (got %d); the LISTEN connection holds one slot for the process lifetime", c.DBMaxConns)
	}
	if c.DBMinConns > c.DBMaxConns {
		return fmt.Errorf("DB_MIN_CONNS (%d) cannot exceed DB_MAX_CONNS (%d)", c.DBMinConns, c.DBMaxConns)
	}
	if c.DBReadURL != "" && c.DBReadMaxConns < 4 {
		return fmt.Errorf("DB_READ_MAX_CONNS must be >= 4 when DB_READ_URL is set (got %d)", c.DBReadMaxConns)
	}
	return nil
}

// checkMailProvider validates the selected mail provider name.
func (c *Config) checkMailProvider() error {
	switch c.MailProvider {
	case "", "smtp", "noop":
		return nil
	default:
		return fmt.Errorf("MAIL_PROVIDER must be one of smtp|noop (got %q)", c.MailProvider)
	}
}

// checkProductionPosture enforces the hardening required when Env is prod.
// It is a no-op outside production.
func (c *Config) checkProductionPosture() error {
	if !c.IsProduction() {
		return nil
	}
	if c.AppRegion == "" || c.AppRegion == "local" {
		return fmt.Errorf("APP_REGION must be set to an explicit deployment region in production")
	}
	migrateURL := strings.TrimSpace(c.DBMigrateURL)
	if migrateURL == "" {
		return fmt.Errorf("DB_MIGRATE_URL must be set in production")
	}
	if migrateURL == strings.TrimSpace(c.DatabaseURL) {
		return fmt.Errorf("DB_MIGRATE_URL must differ from DATABASE_URL in production")
	}
	if len(c.CORSAllowedOrigins) == 0 {
		return fmt.Errorf("CORS_ALLOWED_ORIGINS must be set in production (no wildcard fallback)")
	}
	for _, o := range c.CORSAllowedOrigins {
		if o == "*" {
			return fmt.Errorf("CORS_ALLOWED_ORIGINS must not contain '*' in production")
		}
	}
	if !c.ControlPanelCookieSecure {
		return fmt.Errorf("CONTROL_PANEL_COOKIE_SECURE must be true in production")
	}
	if c.ControlPanelBaseURL == "" {
		return fmt.Errorf("CONTROL_PANEL_BASE_URL must be set in production")
	}
	if !strings.HasPrefix(c.ControlPanelBaseURL, "https://") {
		return fmt.Errorf("CONTROL_PANEL_BASE_URL must use HTTPS in production (got %q)", c.ControlPanelBaseURL)
	}
	if c.ControlPanelEnabled && c.ControlPanelBootstrapTokenFile == "" {
		return fmt.Errorf("CONTROL_PANEL_BOOTSTRAP_TOKEN_FILE must be set in production when control panel is enabled")
	}
	if c.JWTSigningKey == "" {
		return fmt.Errorf("JWT_SIGNING_KEY must be set in production")
	}
	if c.EmailVerifySigningKey == "" {
		return fmt.Errorf("EMAIL_VERIFY_SIGNING_KEY must be set in production")
	}
	if !c.MetricsAuthDisabled && c.MetricsAuthToken == "" {
		return fmt.Errorf("METRICS_AUTH_TOKEN (or _FILE) must be set in production; set METRICS_AUTH_DISABLED=true to explicitly serve /metrics unauthenticated")
	}
	if c.FleetBackend == "docker" && !c.DockerRequireDigest {
		return fmt.Errorf("DOCKER_REQUIRE_DIGEST must be true in production when FLEET_BACKEND=docker")
	}
	if c.FleetBackend == "docker" && !hasNonBlankValue(c.DockerRegistryAllowlist) {
		return fmt.Errorf("DOCKER_REGISTRY_ALLOWLIST must be set in production when FLEET_BACKEND=docker")
	}
	if c.FleetBackend == "docker" && isLoopbackOrLinkLocal(c.GameServerPublicIP) {
		return fmt.Errorf("GAME_SERVER_PUBLIC_IP %q must be a routable address in production", c.GameServerPublicIP)
	}
	return nil
}

func hasNonBlankValue(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// checkAgonesAuth validates the Agones/k3s credential combination. The three
// k3s vars are all-or-nothing and mutually exclusive with a kubeconfig.
func (c *Config) checkAgonesAuth() error {
	if c.FleetBackend != "agones" {
		return nil
	}
	anyK3s := c.K3sAPIURL != "" || c.K3sSAToken != "" || c.K3sCACertB64 != ""
	allK3s := c.K3sAPIURL != "" && c.K3sSAToken != "" && c.K3sCACertB64 != ""
	switch {
	case anyK3s && !allK3s:
		return fmt.Errorf("K3S_API_URL, K3S_SA_TOKEN, K3S_CA_CERT_B64 must all be set together (or all unset)")
	case allK3s && c.AgonesKubeconfig != "":
		return fmt.Errorf("AGONES_KUBECONFIG and K3S_* env vars are mutually exclusive; unset one")
	case c.IsProduction() && allK3s && !strings.HasPrefix(c.K3sAPIURL, "https://"):
		return fmt.Errorf("K3S_API_URL must use HTTPS in production (got %q)", c.K3sAPIURL)
	}
	return nil
}

// warnLowDBPool logs a non-fatal hint when the pool is small for a deployment
// that also runs the matchmaker (which parks the LISTEN socket on one slot).
func (c *Config) warnLowDBPool() {
	if c.DBMaxConns < 8 && c.FleetBackend != "" {
		slog.Warn("DB_MAX_CONNS is low for an enabled matchmaker", "value", c.DBMaxConns,
			"hint", "the LISTEN socket holds one slot, leaving DB_MAX_CONNS-1 for request traffic + cleanup")
	}
}

// checkFields runs the single-field range and enum checks that the hand-written
// setter closures used to perform. It is called only from Load, never from
// Validate: Validate is exercised by tests with sparse configs whose
// zero-valued durations and counts would trip the positivity checks below.
func (c *Config) checkFields() error {
	for _, d := range []struct {
		name string
		val  time.Duration
	}{
		{"MATCHMAKER_INTERVAL", c.MatchmakerInterval},
		{"MATCHMAKER_CLAIM_TTL", c.MatchmakerClaimTTL},
		{"MATCHMAKER_SWEEP_INTERVAL", c.MatchmakerSweepInterval},
		{"RELAY_CRED_TTL", c.RelayCredTTL},
		{"DB_MAX_CONN_LIFETIME", c.DBMaxConnLifetime},
		{"DB_MAX_CONN_IDLE_TIME", c.DBMaxConnIdleTime},
		{"DB_STATEMENT_TIMEOUT", c.DBStatementTimeout},
		{"HTTP_REQUEST_TIMEOUT", c.HTTPRequestTimeout},
	} {
		if d.val <= 0 {
			return fmt.Errorf("%s %q: must be a positive duration", d.name, d.val)
		}
	}

	for _, d := range []struct {
		name string
		val  time.Duration
	}{
		{"MATCHMAKER_RELAX_AFTER", c.MatchmakerRelaxAfter},
		{"MATCHMAKER_REGION_RELAX_AFTER", c.MatchmakerRegionRelaxAfter},
		{"MATCHMAKER_TICKET_TTL", c.MatchmakerTicketTTL},
	} {
		if d.val < 0 {
			return fmt.Errorf("%s %q: must be a non-negative duration", d.name, d.val)
		}
	}

	for _, n := range []struct {
		name string
		val  int64
	}{
		{"MATCHMAKER_MAX_ATTEMPTS", int64(c.MatchmakerMaxAttempts)},
		{"MATCHMAKER_WORKER_COUNT", int64(c.MatchmakerWorkerCount)},
		{"DB_MAX_CONNS", int64(c.DBMaxConns)},
		{"STORAGE_MAX_VALUE_BYTES", c.StorageMaxValueBytes},
	} {
		if n.val <= 0 {
			return fmt.Errorf("%s %d: must be a positive integer", n.name, n.val)
		}
	}

	for _, n := range []struct {
		name string
		val  int64
	}{
		{"REALTIME_MAX_PER_TENANT", c.RealtimeMaxPerTenant},
		{"REALTIME_MAX_PER_PLAYER", c.RealtimeMaxPerPlayer},
		{"MATCHMAKER_MAX_TICKETS_PER_PLAYER", int64(c.MatchmakerMaxTicketsPerPlayer)},
		{"DB_MIN_CONNS", int64(c.DBMinConns)},
		{"DOCKER_DEFAULT_MEMORY", c.DockerDefaultMemory},
		{"DOCKER_DEFAULT_PIDS", c.DockerDefaultPids},
	} {
		if n.val < 0 {
			return fmt.Errorf("%s %d: must be a non-negative integer", n.name, n.val)
		}
	}

	if c.DockerDefaultCPUs < 0 {
		return fmt.Errorf("DOCKER_DEFAULT_CPUS %v: must be a non-negative number", c.DockerDefaultCPUs)
	}

	if c.RelayUDPPort <= 0 || c.RelayUDPPort > 65535 {
		return fmt.Errorf("RELAY_UDP_PORT %d: must be 1..65535", c.RelayUDPPort)
	}
	if c.AppRegion == "" || strings.TrimSpace(c.AppRegion) != c.AppRegion || len(c.AppRegion) > 64 {
		return fmt.Errorf("APP_REGION %q: must be 1..64 characters with no surrounding whitespace", c.AppRegion)
	}

	switch c.SMTPTLS {
	case "off", "starttls", "implicit":
	default:
		return fmt.Errorf("SMTP_TLS %q: must be one of off|starttls|implicit", c.SMTPTLS)
	}

	if err := checkTwoFactorKey(c.TwoFactorEncKey); err != nil {
		return err
	}
	if err := checkEmailVerifySigningKey(c.EmailVerifySigningKey); err != nil {
		return err
	}

	for _, cidr := range c.TrustedProxyCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("TRUSTED_PROXY_CIDRS %q: %w", cidr, err)
		}
	}

	return nil
}

// checkTwoFactorKey accepts an empty key (zero-config auto-generation) or an
// exactly-32-byte hex string; anything else fails startup.
func checkTwoFactorKey(key string) error {
	return checkExactHexKey("TWO_FACTOR_ENC_KEY", key, 32)
}

func checkEmailVerifySigningKey(key string) error {
	return checkExactHexKey("EMAIL_VERIFY_SIGNING_KEY", key, 32)
}

func checkExactHexKey(name, key string, wantBytes int) error {
	if key == "" {
		return nil
	}
	raw, err := hex.DecodeString(key)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if len(raw) != wantBytes {
		return fmt.Errorf("%s: need %d bytes of hex, got %d", name, wantBytes, len(raw))
	}
	return nil
}
