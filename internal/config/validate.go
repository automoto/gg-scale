package config

import (
	"fmt"
	"log/slog"
	"net"
	"strings"
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

// Validate runs cross-field checks that no single var-decl can express.
// Called by Load after every individual setter has run. Returning an
// error blocks startup so a misconfigured production deployment fails
// loud at boot rather than silently weakening its own posture.
//
// All checks here are defence-in-depth: any single failure means the
// operator has explicitly opted into an unsafe configuration, and we
// would rather refuse to run than serve traffic from one.
func (c *Config) Validate() error {
	prod := strings.EqualFold(c.Env, "production") || strings.EqualFold(c.Env, "prod")

	if c.RelaySharedSecret != "" && len(c.RelaySharedSecret) < 32 {
		return fmt.Errorf("RELAY_SHARED_SECRET must be >= 32 bytes when set (got %d)", len(c.RelaySharedSecret))
	}

	if c.MetricsAuthToken != "" && len(c.MetricsAuthToken) < 32 {
		return fmt.Errorf("METRICS_AUTH_TOKEN must be >= 32 bytes when set (got %d)", len(c.MetricsAuthToken))
	}

	// Feature kill switches default off. Configuring a feature while its switch
	// is off is a contradiction we refuse to boot with, rather than silently
	// leaving the feature dark.
	if !c.FeatureP2PRelayEnabled && (c.RelayPublicIP != "" || c.RelaySharedSecret != "") {
		return fmt.Errorf("RELAY_PUBLIC_IP/RELAY_SHARED_SECRET set while FEATURE_P2P_RELAY_ENABLED is false; enable the feature or clear the relay config")
	}
	if !c.FeatureFleetEnabled && c.FleetBackend != "" {
		return fmt.Errorf("FLEET_BACKEND=%q set while FEATURE_FLEET_ENABLED is false; enable the feature or unset FLEET_BACKEND", c.FleetBackend)
	}

	if c.DBMaxConns < 4 {
		return fmt.Errorf("DB_MAX_CONNS must be >= 4 (got %d); the LISTEN connection holds one slot for the process lifetime", c.DBMaxConns)
	}
	if c.DBMinConns > c.DBMaxConns {
		return fmt.Errorf("DB_MIN_CONNS (%d) cannot exceed DB_MAX_CONNS (%d)", c.DBMinConns, c.DBMaxConns)
	}

	switch c.MailProvider {
	case "", "smtp", "noop":
	default:
		return fmt.Errorf("MAIL_PROVIDER must be one of smtp|noop (got %q)", c.MailProvider)
	}

	if prod {
		if len(c.CORSAllowedOrigins) == 0 {
			return fmt.Errorf("CORS_ALLOWED_ORIGINS must be set in production (no wildcard fallback)")
		}
		for _, o := range c.CORSAllowedOrigins {
			if o == "*" {
				return fmt.Errorf("CORS_ALLOWED_ORIGINS must not contain '*' in production")
			}
		}
		if !c.DashboardCookieSecure {
			return fmt.Errorf("DASHBOARD_COOKIE_SECURE must be true in production")
		}
		if c.DashboardBaseURL == "" {
			return fmt.Errorf("DASHBOARD_BASE_URL must be set in production")
		}
		if !strings.HasPrefix(c.DashboardBaseURL, "https://") {
			return fmt.Errorf("DASHBOARD_BASE_URL must use HTTPS in production (got %q)", c.DashboardBaseURL)
		}
		if c.DashboardEnabled && c.DashboardBootstrapTokenFile == "" {
			return fmt.Errorf("DASHBOARD_BOOTSTRAP_TOKEN_FILE must be set in production when dashboard is enabled")
		}
		if c.JWTSigningKey == "" {
			return fmt.Errorf("JWT_SIGNING_KEY must be set in production")
		}
		if !c.MetricsAuthDisabled && c.MetricsAuthToken == "" {
			return fmt.Errorf("METRICS_AUTH_TOKEN (or _FILE) must be set in production; set METRICS_AUTH_DISABLED=true to explicitly serve /metrics unauthenticated")
		}
		if c.FleetBackend == "docker" && !c.DockerRequireDigest {
			return fmt.Errorf("DOCKER_REQUIRE_DIGEST must be true in production when FLEET_BACKEND=docker")
		}
		if c.FleetBackend == "docker" && isLoopbackOrLinkLocal(c.GameServerPublicIP) {
			return fmt.Errorf("GAME_SERVER_PUBLIC_IP %q must be a routable address in production", c.GameServerPublicIP)
		}
	}

	if c.DBMaxConns < 8 && c.FleetBackend != "" {
		slog.Warn("DB_MAX_CONNS is low for an enabled matchmaker", "value", c.DBMaxConns,
			"hint", "the LISTEN socket holds one slot, leaving DB_MAX_CONNS-1 for request traffic + cleanup")
	}

	if c.FleetBackend == "agones" {
		anyK3s := c.K3sAPIURL != "" || c.K3sSAToken != "" || c.K3sCACertB64 != ""
		allK3s := c.K3sAPIURL != "" && c.K3sSAToken != "" && c.K3sCACertB64 != ""
		if anyK3s && !allK3s {
			return fmt.Errorf("K3S_API_URL, K3S_SA_TOKEN, K3S_CA_CERT_B64 must all be set together (or all unset)")
		}
		if allK3s && c.AgonesKubeconfig != "" {
			return fmt.Errorf("AGONES_KUBECONFIG and K3S_* env vars are mutually exclusive; unset one")
		}
		if prod && allK3s && !strings.HasPrefix(c.K3sAPIURL, "https://") {
			return fmt.Errorf("K3S_API_URL must use HTTPS in production (got %q)", c.K3sAPIURL)
		}
	}

	return nil
}
