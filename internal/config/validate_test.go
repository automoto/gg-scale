package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/config"
)

func baseProd() *config.Config {
	return &config.Config{
		Env:                            "production",
		DatabaseURL:                    "postgres://ggscale_app_login@db/ggscale",
		DBMigrateURL:                   "postgres://ggscale_owner@db/ggscale",
		DBMaxConns:                     10,
		DBMinConns:                     2,
		DBMaxConnLifetime:              time.Hour,
		CORSAllowedOrigins:             []string{"https://app.example.com"},
		ControlPanelEnabled:            true,
		ControlPanelBootstrapTokenFile: "/run/secrets/ggscale-bootstrap",
		ControlPanelCookieSecure:       true,
		ControlPanelBaseURL:            "https://control-panel.example.com",
		JWTSigningKey:                  "1234567890abcdef1234567890abcdef",
		EmailVerifySigningKey:          "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		MetricsAuthToken:               "1234567890abcdef1234567890abcdef",
		FleetBackend:                   "agones",
		FeatureFleetEnabled:            true,
	}
}

func TestValidateAcceptsCleanProdConfig(t *testing.T) {
	require.NoError(t, baseProd().Validate())
}

func TestValidateRequiresSeparateDatabaseCredentialsInProd(t *testing.T) {
	tests := []struct {
		name           string
		migrateURL     string
		wantErrMessage string
	}{
		{
			name:           "missing migration URL",
			wantErrMessage: "DB_MIGRATE_URL must be set in production",
		},
		{
			name:           "migration URL matches runtime URL",
			migrateURL:     "postgres://ggscale_app_login@db/ggscale",
			wantErrMessage: "DB_MIGRATE_URL must differ from DATABASE_URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := baseProd()
			c.DBMigrateURL = tt.migrateURL

			err := c.Validate()

			assert.ErrorContains(t, err, tt.wantErrMessage)
		})
	}
}

func TestValidateRejectsShortRelaySecret(t *testing.T) {
	c := baseProd()
	c.RelaySharedSecret = "short"
	err := c.Validate()
	assert.ErrorContains(t, err, "RELAY_SHARED_SECRET")
}

func TestValidateRequiresCORSInProd(t *testing.T) {
	c := baseProd()
	c.CORSAllowedOrigins = nil
	err := c.Validate()
	assert.ErrorContains(t, err, "CORS_ALLOWED_ORIGINS")
}

func TestValidateRejectsWildcardCORSInProd(t *testing.T) {
	c := baseProd()
	c.CORSAllowedOrigins = []string{"*"}
	err := c.Validate()
	assert.ErrorContains(t, err, "must not contain '*'")
}

func TestValidateRequiresSecureCookieInProd(t *testing.T) {
	c := baseProd()
	c.ControlPanelCookieSecure = false
	err := c.Validate()
	assert.ErrorContains(t, err, "CONTROL_PANEL_COOKIE_SECURE")
}

func TestValidateRequiresHTTPSControlPanelBaseURLInProd(t *testing.T) {
	c := baseProd()
	c.ControlPanelBaseURL = "http://control-panel.example.com"
	err := c.Validate()
	assert.ErrorContains(t, err, "HTTPS")
}

func TestValidateRequiresJWTKeyInProd(t *testing.T) {
	c := baseProd()
	c.JWTSigningKey = ""
	err := c.Validate()
	assert.ErrorContains(t, err, "JWT_SIGNING_KEY")
}

func TestValidateRequiresEmailVerifySigningKeyInProd(t *testing.T) {
	c := baseProd()
	c.EmailVerifySigningKey = ""

	err := c.Validate()

	assert.ErrorContains(t, err, "EMAIL_VERIFY_SIGNING_KEY")
}

func TestValidateRequiresBootstrapTokenFileInProd(t *testing.T) {
	c := baseProd()
	c.ControlPanelBootstrapTokenFile = ""
	err := c.Validate()
	assert.ErrorContains(t, err, "CONTROL_PANEL_BOOTSTRAP_TOKEN_FILE")
}

func TestValidateRequiresMetricsTokenInProd(t *testing.T) {
	c := baseProd()
	c.MetricsAuthToken = ""
	err := c.Validate()
	assert.ErrorContains(t, err, "METRICS_AUTH_TOKEN")
}

func TestValidateAcceptsDisabledMetricsAuthInProd(t *testing.T) {
	c := baseProd()
	c.MetricsAuthToken = ""
	c.MetricsAuthDisabled = true
	require.NoError(t, c.Validate())
}

func TestValidateRejectsShortMetricsToken(t *testing.T) {
	c := baseProd()
	c.MetricsAuthToken = "short"
	err := c.Validate()
	assert.ErrorContains(t, err, "METRICS_AUTH_TOKEN must be >= 32 bytes")
}

func TestValidateRequiresDigestPinForDockerProd(t *testing.T) {
	c := baseProd()
	c.FleetBackend = "docker"
	c.DockerRequireDigest = false
	err := c.Validate()
	assert.ErrorContains(t, err, "DOCKER_REQUIRE_DIGEST")
}

func TestValidateAcceptsDigestPinForDockerProd(t *testing.T) {
	c := baseProd()
	c.FleetBackend = "docker"
	c.DockerRequireDigest = true
	c.DockerRegistryAllowlist = []string{"ghcr.io/acme"}
	assert.NoError(t, c.Validate())
}

func TestValidateRequiresRegistryAllowlistForDockerProd(t *testing.T) {
	tests := []struct {
		name      string
		allowlist []string
	}{
		{name: "missing"},
		{name: "blank", allowlist: []string{"  "}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := baseProd()
			c.FleetBackend = "docker"
			c.DockerRequireDigest = true
			c.DockerRegistryAllowlist = tc.allowlist

			err := c.Validate()

			assert.ErrorContains(t, err, "DOCKER_REGISTRY_ALLOWLIST")
		})
	}
}

func TestValidateRequiresPoolMinimum(t *testing.T) {
	c := baseProd()
	c.DBMaxConns = 2
	err := c.Validate()
	assert.ErrorContains(t, err, "DB_MAX_CONNS")
}

func TestValidateRejectsUnknownMailProvider(t *testing.T) {
	c := baseProd()
	c.MailProvider = "mystery"
	err := c.Validate()
	assert.ErrorContains(t, err, "MAIL_PROVIDER")
}

func TestValidateRejectsRelayConfiguredWhileFeatureOff(t *testing.T) {
	c := baseProd()
	c.FeatureP2PRelayEnabled = false
	c.RelaySharedSecret = strings.Repeat("a", 32)
	c.RelayPublicIP = "203.0.113.10"
	err := c.Validate()
	assert.ErrorContains(t, err, "FEATURE_P2P_RELAY_ENABLED")
}

func TestValidateAcceptsRelayConfiguredWithFeatureOn(t *testing.T) {
	c := baseProd()
	c.FeatureP2PRelayEnabled = true
	c.RelaySharedSecret = strings.Repeat("a", 32)
	c.RelayPublicIP = "203.0.113.10"
	assert.NoError(t, c.Validate())
}

func TestValidateRejectsFleetBackendWhileFeatureOff(t *testing.T) {
	c := baseProd()
	c.FeatureFleetEnabled = false
	c.FleetBackend = "docker"
	c.DockerRequireDigest = true
	c.GameServerPublicIP = "203.0.113.5"
	err := c.Validate()
	assert.ErrorContains(t, err, "FEATURE_FLEET_ENABLED")
}

func TestValidateAllowsDevWithoutCORS(t *testing.T) {
	c := &config.Config{
		Env:        "dev",
		DBMaxConns: 4,
	}
	assert.NoError(t, c.Validate())
}
