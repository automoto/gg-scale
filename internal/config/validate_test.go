package config_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/config"
)

func baseProd() *config.Config {
	return &config.Config{
		Env:                            "production",
		AppRegion:                      "us-east",
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

func TestValidateRequiresExplicitRegionInProduction(t *testing.T) {
	c := baseProd()
	c.AppRegion = "local"

	err := c.Validate()

	assert.ErrorContains(t, err, "APP_REGION")
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

func TestValidate_should_accept_relay_issuer_without_relay_urls(t *testing.T) {
	c := baseProd()
	c.FeatureP2PRelayEnabled = true
	c.RelaySharedSecret = strings.Repeat("a", 32)

	err := c.Validate()

	assert.NoError(t, err)
}

func TestValidate_should_accept_embedded_relay_urls(t *testing.T) {
	c := baseProd()
	c.FeatureP2PRelayEnabled = true
	c.RelaySharedSecret = strings.Repeat("a", 32)
	c.RelayPublicIP = "203.0.113.10"
	c.RelayEmbeddedURLs = []string{"turn:relay.example.com:3479?transport=udp"}

	err := c.Validate()

	assert.NoError(t, err)
}

func TestValidate_should_reject_malformed_embedded_relay_urls(t *testing.T) {
	c := baseProd()
	c.FeatureP2PRelayEnabled = true
	c.RelaySharedSecret = strings.Repeat("a", 32)
	c.RelayPublicIP = "203.0.113.10"
	c.RelayEmbeddedURLs = []string{"https://relay.example.com"}

	err := c.Validate()

	assert.ErrorContains(t, err, "RELAY_EMBEDDED_URLS")
}

func TestValidate_should_reject_embedded_relay_urls_without_public_ip(t *testing.T) {
	c := baseProd()
	c.FeatureP2PRelayEnabled = true
	c.RelaySharedSecret = strings.Repeat("a", 32)
	c.RelayEmbeddedURLs = []string{"turn:relay.example.com:3479?transport=udp"}

	err := c.Validate()

	assert.ErrorContains(t, err, "RELAY_PUBLIC_IP")
}

func TestValidate_should_reject_short_platform_admin_token(t *testing.T) {
	c := baseProd()
	c.PlatformAdminToken = "short"

	err := c.Validate()

	assert.ErrorContains(t, err, "PLATFORM_ADMIN_TOKEN")
}

func TestValidate_should_reject_short_relay_probe_token(t *testing.T) {
	c := baseProd()
	c.RelayProbeToken = "short"

	err := c.Validate()

	assert.ErrorContains(t, err, "RELAY_PROBE_TOKEN")
}

func TestValidateRejectsRelayHalfPortRange(t *testing.T) {
	c := baseProd()
	c.FeatureP2PRelayEnabled = true
	c.RelaySharedSecret = strings.Repeat("a", 32)
	c.RelayPublicIP = "203.0.113.10"
	c.RelayMinPort = 49200 // max unset
	err := c.Validate()
	assert.ErrorContains(t, err, "RELAY_MAX_PORT")
}

func TestValidateAcceptsRelayPortRange(t *testing.T) {
	c := baseProd()
	c.FeatureP2PRelayEnabled = true
	c.RelaySharedSecret = strings.Repeat("a", 32)
	c.RelayPublicIP = "203.0.113.10"
	c.RelayMinPort = 49200
	c.RelayMaxPort = 49300
	assert.NoError(t, c.Validate())
}

func TestValidateAcceptsSTUNSchemes(t *testing.T) {
	for _, stun := range []string{"stun:relay.example.com:3478", "stuns:relay.example.com:5349"} {
		t.Run(stun, func(t *testing.T) {
			c := baseProd()
			c.FeatureP2PRelayEnabled = true
			c.RelaySharedSecret = strings.Repeat("a", 32)
			c.RelayPublicIP = "203.0.113.10"
			c.RelaySTUNURLs = []string{stun}
			assert.NoError(t, c.Validate())
		})
	}
}

func TestValidateRejectsRelayMalformedSTUNURL(t *testing.T) {
	c := baseProd()
	c.FeatureP2PRelayEnabled = true
	c.RelaySharedSecret = strings.Repeat("a", 32)
	c.RelayPublicIP = "203.0.113.10"
	c.RelaySTUNURLs = []string{"https://relay.example.com"}
	err := c.Validate()
	assert.ErrorContains(t, err, "RELAY_STUN_URLS")
}

func TestValidateRejectsFleetBackendWhileFeatureOff(t *testing.T) {
	c := baseProd()
	c.FeatureFleetEnabled = false
	c.FleetBackend = "agones"
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

func TestValidateSSO(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *config.Config)
		wantErr string
	}{
		{
			name: "player Google client ID without secret",
			mutate: func(c *config.Config) {
				c.PlayerSSOGoogleClientID = "id"
			},
			wantErr: "PLAYER_SSO_GOOGLE_CLIENT_ID and PLAYER_SSO_GOOGLE_CLIENT_SECRET must be set together",
		},
		{
			name: "control panel Google secret without client ID",
			mutate: func(c *config.Config) {
				c.ControlPanelSSOGoogleClientSecret = "secret"
			},
			wantErr: "CONTROL_PANEL_SSO_GOOGLE_CLIENT_ID and CONTROL_PANEL_SSO_GOOGLE_CLIENT_SECRET must be set together",
		},
		{
			name: "provider on without base URL",
			mutate: func(c *config.Config) {
				c.Env = "dev"
				c.ControlPanelBaseURL = ""
				c.PlayerSSOGoogleClientID = "id"
				c.PlayerSSOGoogleClientSecret = "secret"
			},
			wantErr: "CONTROL_PANEL_BASE_URL must be set",
		},
		{
			name: "provider on with relative base URL",
			mutate: func(c *config.Config) {
				c.Env = "dev"
				c.ControlPanelBaseURL = "/control-panel"
				c.PlayerSSOGoogleClientID = "id"
				c.PlayerSSOGoogleClientSecret = "secret"
			},
			wantErr: "must be an absolute http(s) URL",
		},
		{
			name: "both surfaces on",
			mutate: func(c *config.Config) {
				c.PlayerSSOGoogleClientID = "id"
				c.PlayerSSOGoogleClientSecret = "secret"
				c.ControlPanelSSOGoogleClientID = "id2"
				c.ControlPanelSSOGoogleClientSecret = "secret2"
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := baseProd()
			tc.mutate(c)

			err := c.Validate()

			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestValidate_should_warn_when_probe_token_equals_lifecycle_token(t *testing.T) {
	logs := captureDefaultLog(t)
	c := baseProd()
	c.LifecycleToken = strings.Repeat("l", 32)
	c.RelayProbeToken = strings.Repeat("l", 32)

	require.NoError(t, c.Validate())

	assert.Contains(t, logs.String(), "RELAY_PROBE_TOKEN equals LIFECYCLE_TOKEN")
}

func TestValidate_should_not_warn_when_probe_token_differs(t *testing.T) {
	logs := captureDefaultLog(t)
	c := baseProd()
	c.LifecycleToken = strings.Repeat("l", 32)
	c.RelayProbeToken = strings.Repeat("p", 32)

	require.NoError(t, c.Validate())

	assert.NotContains(t, logs.String(), "RELAY_PROBE_TOKEN equals")
}
