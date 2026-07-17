//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/controlpanel"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/tenant"
)

const branchPlayerLimit = int64(250_000)

func tenantProjectCreateURL(base string, tenantID int64) string {
	return fmt.Sprintf("%s/v1/control-panel/tenants/%d/projects", base, tenantID)
}

func createProjectRequest(t *testing.T, client *http.Client, target, csrf, name string, cookie *http.Cookie) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(url.Values{
		"_csrf": {csrf},
		"name":  {name},
	}.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, string(body)
}

func TestBranchFollowup_direct_tenant_provisioning_respects_enforcement_config(t *testing.T) {
	for _, enforce := range []bool{false, true} {
		enforce := enforce
		t.Run(strconv.FormatBool(enforce), func(t *testing.T) {
			c := startCluster(t)
			adminID := seedControlPanelUser(t, c, "provision-admin@example.test", "correct-horse-battery-staple", true)
			_ = adminID
			srv, _ := newControlPanelAndPlayerServerWithConfig(t, c, controlpanel.Config{
				Mount:                  true,
				BaseURL:                "http://app.example.test",
				MailFrom:               "no-reply@example.test",
				EnforceNewTenantQuotas: enforce,
			})
			cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL,
				"provision-admin@example.test", "correct-horse-battery-staple")

			name := "provision-" + strconv.FormatBool(enforce)
			resp := postForm(t, noRedirectClient(), srv.URL+"/v1/control-panel/tenants", url.Values{
				"_csrf":        {csrf},
				"tenant_name":  {name},
				"project_name": {"starter"},
				"label":        {"first key"},
			}, cookie)
			resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)

			var tenantID int64
			var got bool
			require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
				`SELECT id, enforce_quotas FROM tenants WHERE name = $1`, name).Scan(&tenantID, &got))
			assert.Equal(t, enforce, got)

			var projects, keys, owners, grouping, audits int
			require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
				`SELECT count(*) FROM projects WHERE tenant_id = $1 AND deleted_at IS NULL`, tenantID).Scan(&projects))
			require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
				`SELECT count(*) FROM api_keys WHERE tenant_id = $1 AND revoked_at IS NULL`, tenantID).Scan(&keys))
			require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
				`SELECT count(*) FROM control_panel_memberships WHERE tenant_id = $1 AND role = 'owner'`, tenantID).Scan(&owners))
			require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
				`SELECT count(*) FROM casbin_rule
				 WHERE ptype = 'g' AND v0 = 'control_panel:user:' || $1::bigint
				   AND v1 = 'role:tenant_owner' AND v2 = 'tenant:' || $2::bigint::text`, adminID, tenantID).Scan(&grouping))
			require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
				`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'control_panel.tenant.created'`, tenantID).Scan(&audits))
			assert.Equal(t, []int{1, 1, 1, 1, 1}, []int{projects, keys, owners, grouping, audits})
		})
	}
}

func TestBranchFollowup_tenant_signup_accept_enforces_new_tenant_quota_config(t *testing.T) {
	c := startCluster(t)
	seedControlPanelUser(t, c, "signup-admin@example.test", "correct-horse-battery-staple", true)
	srv, rec := newControlPanelAndPlayerServerWithConfig(t, c, controlpanel.Config{
		Mount:                  true,
		BaseURL:                "http://app.example.test",
		MailFrom:               "no-reply@example.test",
		EnforceNewTenantQuotas: true,
	})
	adminCookie, adminCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"signup-admin@example.test", "correct-horse-battery-staple")
	client := noRedirectClient()
	resp := postForm(t, client, srv.URL+"/v1/control-panel/admin/tenant-signups/config",
		url.Values{"_csrf": {adminCSRF}, "enabled": {"on"}}, adminCookie)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	dev := jarClient(t)
	csrf := getPlayerCSRF(t, dev, srv.URL+"/v1/control-panel/request-access")
	resp = postForm(t, dev, srv.URL+"/v1/control-panel/request-access", url.Values{
		"_csrf":               {csrf},
		"email":               {"quota-signup@example.test"},
		"tenant_name":         {"quota-signup-tenant"},
		"project_description": {"quota branch test"},
	}, nil)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var requestID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM tenant_signup_requests WHERE email = 'quota-signup@example.test'`).Scan(&requestID))
	resp = postForm(t, client,
		srv.URL+"/v1/control-panel/admin/tenant-signups/"+strconv.FormatInt(requestID, 10)+"/approve",
		url.Values{"_csrf": {adminCSRF}, "tenant_name": {"quota-signup-tenant"}}, adminCookie)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.NotEmpty(t, rec.Sent)
	m := signupCodeRe.FindStringSubmatch(rec.Sent[len(rec.Sent)-1].Body)
	require.Len(t, m, 2)
	code, err := url.QueryUnescape(m[1])
	require.NoError(t, err)

	accept := jarClient(t)
	acceptURL := srv.URL + "/v1/control-panel/request-access/accept?code=" + url.QueryEscape(code)
	acceptCSRF := getPlayerCSRF(t, accept, acceptURL)
	resp = postForm(t, accept, srv.URL+"/v1/control-panel/request-access/accept", url.Values{
		"_csrf":    {acceptCSRF},
		"code":     {code},
		"password": {"correct-horse-battery-staple"},
	}, nil)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	var enforce bool
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT enforce_quotas FROM tenants WHERE name = 'quota-signup-tenant'`).Scan(&enforce))
	assert.True(t, enforce)
}

func TestBranchFollowup_config_changes_do_not_rewrite_existing_tenants(t *testing.T) {
	c := startCluster(t)
	tenantFalse, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "existing-false")
	tenantTrue, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "existing-true")
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE tenants SET enforce_quotas = (id = $2) WHERE id IN ($1, $2)`, tenantFalse, tenantTrue)
	require.NoError(t, err)

	for _, enforce := range []bool{true, false} {
		srv, _ := newControlPanelAndPlayerServerWithConfig(t, c, controlpanel.Config{
			Mount:                  true,
			BaseURL:                "http://app.example.test",
			MailFrom:               "no-reply@example.test",
			EnforceNewTenantQuotas: enforce,
		})
		resp, err := http.Get(srv.URL + "/v1/healthz")
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	var gotFalse, gotTrue bool
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT enforce_quotas FROM tenants WHERE id = $1`, tenantFalse).Scan(&gotFalse))
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT enforce_quotas FROM tenants WHERE id = $1`, tenantTrue).Scan(&gotTrue))
	assert.False(t, gotFalse)
	assert.True(t, gotTrue)
}

func TestBranchFollowup_project_quota_exact_and_concurrent(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "project-quota")
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE tenants SET enforce_quotas = true WHERE id = $1`, tenantID)
	require.NoError(t, err)
	_, err = c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'second')`, tenantID)
	require.NoError(t, err)
	seedControlPanelUser(t, c, "project-admin@example.test", "correct-horse-battery-staple", true)
	srv, _ := newControlPanelAndPlayerServer(t, c)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"project-admin@example.test", "correct-horse-battery-staple")
	target := tenantProjectCreateURL(srv.URL, tenantID)

	const attempts = 20
	var successes, rejected atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, body := createProjectRequest(t, noRedirectClient(), target, csrf,
				fmt.Sprintf("race-%02d", i), cookie)
			switch resp.StatusCode {
			case http.StatusSeeOther:
				successes.Add(1)
			case http.StatusConflict:
				require.Contains(t, body, "project limit")
				rejected.Add(1)
			default:
				t.Errorf("unexpected status %d: %s", resp.StatusCode, body)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	assert.Equal(t, int64(1), successes.Load())
	assert.Equal(t, int64(attempts-1), rejected.Load())

	var count int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM projects WHERE tenant_id = $1 AND deleted_at IS NULL`, tenantID).Scan(&count))
	assert.Equal(t, int64(3), count)
}

func TestBranchFollowup_project_quota_unenforced_and_tier3_are_unlimited(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tier    int16
		enforce bool
		count   int
	}{
		{"unenforced", 0, false, 5},
		{"tier3", 3, true, 21},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := startCluster(t)
			tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, tc.tier, "projects-"+tc.name)
			_, err := c.bootstrapPool.Exec(context.Background(),
				`UPDATE tenants SET enforce_quotas = $2 WHERE id = $1`, tenantID, tc.enforce)
			require.NoError(t, err)
			seedControlPanelUser(t, c, "projects-"+tc.name+"@example.test", "correct-horse-battery-staple", true)
			srv, _ := newControlPanelAndPlayerServer(t, c)
			cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL,
				"projects-"+tc.name+"@example.test", "correct-horse-battery-staple")
			for i := 1; i < tc.count; i++ {
				resp, body := createProjectRequest(t, noRedirectClient(), tenantProjectCreateURL(srv.URL, tenantID),
					csrf, fmt.Sprintf("project-%02d", i), cookie)
				require.Equal(t, http.StatusSeeOther, resp.StatusCode, body)
			}
			var count int
			require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
				`SELECT count(*) FROM projects WHERE tenant_id = $1 AND deleted_at IS NULL`, tenantID).Scan(&count))
			assert.Equal(t, tc.count, count)
		})
	}
}

func fillPlayersTo(t *testing.T, c *cluster, tenantID, projectID, target int64, prefix string) {
	t.Helper()
	var current int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM project_players WHERE tenant_id = $1 AND deleted_at IS NULL`, tenantID).Scan(&current))
	require.LessOrEqual(t, current, target)
	missing := target - current
	if missing == 0 {
		return
	}
	_, err := c.bootstrapPool.Exec(context.Background(), `
		INSERT INTO project_players (tenant_id, project_id, external_id)
		SELECT $1, $2, $3 || '-' || g::text
		FROM generate_series(1, $4::bigint) AS g`, tenantID, projectID, prefix, missing)
	require.NoError(t, err)
}

func signCustomToken(t *testing.T, secret []byte, externalID string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"external_id": externalID,
		"exp":         time.Now().Add(time.Hour).Unix(),
		"aud":         "ggscale-custom-token",
	})
	signed, err := tok.SignedString(secret)
	require.NoError(t, err)
	return signed
}

func TestBranchFollowup_player_quota_caps_all_growth_and_preserves_existing_paths(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "player-quota")
	secret := []byte("branch-player-quota-custom-secret")
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE tenants SET enforce_quotas = true, custom_token_secret = $2 WHERE id = $1`, tenantID, secret)
	require.NoError(t, err)
	adminID := seedControlPanelUser(t, c, "player-quota-admin@example.test", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")
	targetPlayer := seedExternalIDOnlyPlayer(t, c, tenantID, projectID, "existing-link-target")

	srv, rec := newControlPanelAndPlayerServer(t, c)
	adminCookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"player-quota-admin@example.test", "correct-horse-battery-staple")

	// Create one custom-token player and capture a refresh token before filling.
	existingToken := signCustomToken(t, secret, "custom-existing")
	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/custom-token", "player-quota",
		map[string]string{"token": existingToken})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var existingSession struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	require.NoError(t, json.Unmarshal(body, &existingSession))

	// Prepare one no-growth link invite and one growth invite before reaching
	// the boundary so invite throttling is not part of the quota assertion.
	linkResp := postLink(t, srv.URL, adminCookie, csrf, tenantID, projectID, targetPlayer,
		"existing-link@example.test")
	linkResp.Body.Close()
	require.Equal(t, http.StatusSeeOther, linkResp.StatusCode)
	invitePath := fmt.Sprintf("%s/v1/control-panel/tenants/%d/projects/%d/players/invite", srv.URL, tenantID, projectID)
	inviteResp := postForm(t, noRedirectClient(), invitePath,
		url.Values{"_csrf": {csrf}, "email": {"new-at-cap@example.test"}}, adminCookie)
	inviteResp.Body.Close()
	require.Equal(t, http.StatusSeeOther, inviteResp.StatusCode)
	require.Len(t, rec.Sent, 2)

	fillPlayersTo(t, c, tenantID, projectID, branchPlayerLimit, "bf-cap")

	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/anonymous", "player-quota", nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "player-quota",
		map[string]string{"email": "signup-at-cap@example.test", "password": "playerpass1"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/custom-token", "player-quota",
		map[string]string{"token": signCustomToken(t, secret, "custom-new-at-cap")})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))

	// Existing identities and sessions remain available at the cap.
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/custom-token", "player-quota",
		map[string]string{"token": existingToken})
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/refresh", "player-quota",
		map[string]string{"refresh_token": existingSession.RefreshToken})
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	resp, body = authedReq(t, http.MethodGet, srv.URL+"/v1/profile", "player-quota", existingSession.AccessToken, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	// Linking the pre-existing row is no-growth and succeeds at the cap.
	status, acceptBody := acceptInvite(t, srv.URL, rec.Sent[0].Body)
	assert.Equal(t, http.StatusSeeOther, status, acceptBody)

	// A plain invite would create a row and must remain pending after rejection.
	status, acceptBody = acceptInvite(t, srv.URL, rec.Sent[1].Body)
	assert.Equal(t, http.StatusForbidden, status, acceptBody)
	assert.Contains(t, acceptBody, "registered-player limit")

	var count, signupRows, newInviteRows int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM project_players WHERE tenant_id = $1 AND deleted_at IS NULL`, tenantID).Scan(&count))
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM project_players WHERE tenant_id = $1 AND email = 'signup-at-cap@example.test'`, tenantID).Scan(&signupRows))
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM project_players WHERE tenant_id = $1 AND email = 'new-at-cap@example.test'`, tenantID).Scan(&newInviteRows))
	assert.Equal(t, branchPlayerLimit, count)
	assert.Zero(t, signupRows)
	assert.Zero(t, newInviteRows)
	var accepted bool
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT accepted_at IS NOT NULL FROM player_invitations
		WHERE tenant_id = $1 AND email = 'new-at-cap@example.test'`, tenantID).Scan(&accepted))
	assert.False(t, accepted)
}

func postJSONStatus(target, apiKey string, body any) (int, error) {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return 0, err
		}
	}
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	err = resp.Body.Close()
	return resp.StatusCode, err
}

type branchAllowAllLimiter struct{}

func (branchAllowAllLimiter) Allow(context.Context, string, float64, float64) (ratelimit.Decision, error) {
	return ratelimit.Decision{Allowed: true}, nil
}

func newQuotaServerWithAllowAllLimiter(t *testing.T, c *cluster) *httptest.Server {
	t.Helper()
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)

	router := httpapi.NewRouter(httpapi.Deps{
		Version: "v1",
		Commit:  "test",
		Pool:    pool,
		Lookup:  tenant.NewSQLLookup(c.appPool),
		Limiter: branchAllowAllLimiter{},
		Signer:  signer,
		Cache:   c.cache,
		RBAC:    authorizer,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func newQuotaMetricsServer(t *testing.T, c *cluster) *httptest.Server {
	t.Helper()
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)
	reg := prometheus.NewRegistry()

	router := httpapi.NewRouter(httpapi.Deps{
		Version:               "v1",
		Commit:                "test",
		Pool:                  pool,
		Lookup:                tenant.NewSQLLookup(c.appPool),
		Limiter:               branchAllowAllLimiter{},
		Signer:                signer,
		Cache:                 c.cache,
		Registry:              reg,
		Metrics:               observability.NewMetrics(reg),
		RBAC:                  authorizer,
		EmailVerifySigningKey: []byte(testEmailVerifySigningKey),
		ControlPanel:          controlpanel.Config{Mount: true, BaseURL: "http://app.example.test"},
		ControlPanelBootstrap: controlpanel.DisabledBootstrap(),
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func TestBranchFollowup_quota_rejection_metrics_cover_all_axes_without_ids(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "quota-metrics")
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE tenants SET enforce_quotas = true WHERE id = $1`, tenantID)
	require.NoError(t, err)
	seedControlPanelUser(t, c, "quota-metrics-pa@example.test", "correct-horse-battery-staple", true)
	srv := newQuotaMetricsServer(t, c)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"quota-metrics-pa@example.test", "correct-horse-battery-staple")
	access := anonymousLogin(t, srv.URL, "quota-metrics")
	storageTarget := srv.URL + "/v1/storage/objects/metrics"
	stored := storageRequestStatus(http.MethodPut, storageTarget, "quota-metrics", access, []byte(`0`), "")
	require.NoError(t, stored.err)
	require.Equal(t, http.StatusOK, stored.status, stored.body)
	_, err = c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'metrics-2'), ($1, 'metrics-3')`, tenantID)
	require.NoError(t, err)
	fillPlayersTo(t, c, tenantID, projectID, branchPlayerLimit, "quota-metrics")
	_, err = c.bootstrapPool.Exec(context.Background(), `
		UPDATE tenant_storage_usage SET total_bytes = $2 WHERE tenant_id = $1`,
		tenantID, int64(5)<<30)
	require.NoError(t, err)

	resp, projectBody := createProjectRequest(t, noRedirectClient(), tenantProjectCreateURL(srv.URL, tenantID),
		csrf, "metrics-blocked", cookie)
	assert.Equal(t, http.StatusConflict, resp.StatusCode, projectBody)
	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/anonymous", "quota-metrics", nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
	growth := storageRequestStatus(http.MethodPut, storageTarget, "quota-metrics", access, []byte(`{"more":true}`), "")
	require.NoError(t, growth.err)
	assert.Equal(t, http.StatusForbidden, growth.status, growth.body)

	metricsResp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	metricsBody, err := io.ReadAll(metricsResp.Body)
	require.NoError(t, err)
	require.NoError(t, metricsResp.Body.Close())
	require.Equal(t, http.StatusOK, metricsResp.StatusCode)
	var quotaLines []string
	for _, line := range strings.Split(string(metricsBody), "\n") {
		if strings.HasPrefix(line, "ggscale_quota_rejections_total{") {
			quotaLines = append(quotaLines, line)
		}
	}
	assert.ElementsMatch(t, []string{
		`ggscale_quota_rejections_total{axis="players"} 1`,
		`ggscale_quota_rejections_total{axis="projects"} 1`,
		`ggscale_quota_rejections_total{axis="storage"} 1`,
	}, quotaLines)
	for _, line := range quotaLines {
		assert.NotContains(t, line, "tenant")
		assert.NotContains(t, line, "player_id")
	}
}

func TestBranchFollowup_player_quota_mixed_concurrency_is_tenant_wide(t *testing.T) {
	c := startCluster(t)
	tenantID, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "player-race-a")
	var projectB int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'player-race-b') RETURNING id`, tenantID).Scan(&projectB))
	seedAPIKey(t, c.bootstrapPool, tenantID, &projectB, "player-race-b", "publishable")
	secret := []byte("branch-player-race-custom-secret")
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE tenants SET enforce_quotas = true, custom_token_secret = $2 WHERE id = $1`, tenantID, secret)
	require.NoError(t, err)
	fillPlayersTo(t, c, tenantID, projectA, branchPlayerLimit/2, "bf-race-a")
	fillPlayersTo(t, c, tenantID, projectB, branchPlayerLimit-1, "bf-race-b")

	srvA := newQuotaServerWithAllowAllLimiter(t, c)
	srvB := newQuotaServerWithAllowAllLimiter(t, c)
	type result struct {
		status int
		err    error
	}
	const attempts = 20
	customTokens := make([]string, attempts)
	for i := 1; i < attempts; i += 2 {
		customTokens[i] = signCustomToken(t, secret, fmt.Sprintf("race-custom-%02d", i))
	}
	results := make(chan result, attempts)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				status, err := postJSONStatus(srvA.URL+"/v1/auth/anonymous", "player-race-a", nil)
				results <- result{status, err}
				return
			}
			status, err := postJSONStatus(srvB.URL+"/v1/auth/custom-token", "player-race-b",
				map[string]string{"token": customTokens[i]})
			results <- result{status, err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	var allowed, denied int
	for got := range results {
		require.NoError(t, got.err)
		switch got.status {
		case http.StatusOK:
			allowed++
		case http.StatusForbidden:
			denied++
		default:
			t.Errorf("unexpected status %d", got.status)
		}
	}
	assert.Equal(t, 1, allowed)
	assert.Equal(t, attempts-1, denied)

	var count int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM project_players WHERE tenant_id = $1 AND deleted_at IS NULL`, tenantID).Scan(&count))
	assert.Equal(t, branchPlayerLimit, count)

	// One soft delete frees exactly one tenant-wide slot.
	_, err = c.bootstrapPool.Exec(context.Background(), `
		UPDATE project_players SET deleted_at = now()
		WHERE id = (SELECT id FROM project_players WHERE tenant_id = $1 AND project_id = $2 AND deleted_at IS NULL LIMIT 1)`,
		tenantID, projectB)
	require.NoError(t, err)
	status, err := postJSONStatus(srvA.URL+"/v1/auth/anonymous", "player-race-a", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)

	// Disabling enforcement restores self-host growth beyond the class limit.
	_, err = c.bootstrapPool.Exec(context.Background(),
		`UPDATE tenants SET enforce_quotas = false WHERE id = $1`, tenantID)
	require.NoError(t, err)
	status, err = postJSONStatus(srvB.URL+"/v1/auth/anonymous", "player-race-b", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM project_players WHERE tenant_id = $1 AND deleted_at IS NULL`, tenantID).Scan(&count))
	assert.Equal(t, branchPlayerLimit+1, count)
}
