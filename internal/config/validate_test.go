package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/config"
)

func baseProd() *config.Config {
	return &config.Config{
		Env:                         "production",
		DBMaxConns:                  10,
		DBMinConns:                  2,
		DBMaxConnLifetime:           time.Hour,
		CORSAllowedOrigins:          []string{"https://app.example.com"},
		DashboardEnabled:            true,
		DashboardBootstrapTokenFile: "/run/secrets/ggscale-bootstrap",
		DashboardCookieSecure:       true,
		DashboardBaseURL:            "https://dashboard.example.com",
		JWTSigningKey:               "1234567890abcdef1234567890abcdef",
		FleetBackend:                "agones",
	}
}

func TestValidateAcceptsCleanProdConfig(t *testing.T) {
	require.NoError(t, baseProd().Validate())
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
	c.DashboardCookieSecure = false
	err := c.Validate()
	assert.ErrorContains(t, err, "DASHBOARD_COOKIE_SECURE")
}

func TestValidateRequiresHTTPSDashboardBaseURLInProd(t *testing.T) {
	c := baseProd()
	c.DashboardBaseURL = "http://dashboard.example.com"
	err := c.Validate()
	assert.ErrorContains(t, err, "HTTPS")
}

func TestValidateRequiresJWTKeyInProd(t *testing.T) {
	c := baseProd()
	c.JWTSigningKey = ""
	err := c.Validate()
	assert.ErrorContains(t, err, "JWT_SIGNING_KEY")
}

func TestValidateRequiresBootstrapTokenFileInProd(t *testing.T) {
	c := baseProd()
	c.DashboardBootstrapTokenFile = ""
	err := c.Validate()
	assert.ErrorContains(t, err, "DASHBOARD_BOOTSTRAP_TOKEN_FILE")
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
	assert.NoError(t, c.Validate())
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

func TestValidateAllowsDevWithoutCORS(t *testing.T) {
	c := &config.Config{
		Env:        "dev",
		DBMaxConns: 4,
	}
	assert.NoError(t, c.Validate())
}
