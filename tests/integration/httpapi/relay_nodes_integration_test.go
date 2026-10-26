//go:build integration

// e2e:bucket a

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/auth"
	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/lifecycle"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/relay"
	"github.com/automoto/gg-scale/internal/relaynode"
	"github.com/automoto/gg-scale/internal/tenant"
)

const (
	relayNodesAdminToken = "platform-admin-token-0123456789abcdef"
	relayNodesProbeToken = "relay-lifecycle-token-0123456789abcdef"
	relayNodesSecret     = "relay-nodes-shared-secret-0123456789abcdef"
)

// relayNodesFixture is an API server with the relay node registry, plus a
// monitor that probes the registry on demand instead of every 10s.
type relayNodesFixture struct {
	c       *cluster
	srv     *httptest.Server
	store   *relaynode.Store
	monitor *relaynode.Monitor
	issuer  *relay.Issuer
	apiKey  string
	access  string
}

func newRelayNodesFixture(t *testing.T, name string) *relayNodesFixture {
	t.Helper()
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, name)
	_, err := c.bootstrapPool.Exec(t.Context(),
		`UPDATE api_keys SET key_type = 'publishable', scopes = ARRAY['p2p_relay']::text[]
		  WHERE tenant_id = $1 AND project_id = $2`,
		tenantID, projectID)
	require.NoError(t, err)
	_, err = c.bootstrapPool.Exec(t.Context(),
		`INSERT INTO feature_grants (tenant_id, project_id, feature, enabled, reason)
		 VALUES ($1, $2, $3, true, 'integration test fixture')`,
		tenantID, projectID, string(rbac.FeatureP2PRelay))
	require.NoError(t, err)

	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)
	store := relaynode.NewStore(pool)
	issuer := relay.NewIssuer(relayNodesSecret, "relay.test", time.Minute)
	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Version:            "v1",
		Commit:             "test",
		Pool:               pool,
		Lookup:             tenant.NewSQLLookup(c.appPool),
		Limiter:            ratelimit.NewCacheLimiter(c.cache),
		Signer:             signer,
		Cache:              c.cache,
		RBAC:               authorizer,
		RelayIssuer:        issuer,
		RelayNodes:         relaynode.NewSelector(store.Eligible, nil),
		RelayNodeAdmin:     store,
		PlatformAdminToken: relayNodesAdminToken,
	}))
	t.Cleanup(srv.Close)
	return &relayNodesFixture{
		c:       c,
		srv:     srv,
		store:   store,
		monitor: relaynode.NewMonitor(store, relaynode.NewProber(relayNodesProbeToken, 200*time.Millisecond), slog.New(slog.DiscardHandler)),
		issuer:  issuer,
		apiKey:  name,
		access:  anonymousLogin(t, srv.URL, name),
	}
}

// register adds a node through the admin API and returns its id.
func (f *relayNodesFixture) register(t *testing.T, region, lifecycleURL string, turnURLs ...string) int64 {
	t.Helper()
	body, err := json.Marshal(map[string]any{"region": region, "turn_urls": turnURLs, "lifecycle_url": lifecycleURL})
	require.NoError(t, err)
	resp, out := f.admin(t, http.MethodPost, "/v1/admin/relay-nodes", string(body))
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(out))
	var n relaynode.Node
	require.NoError(t, json.Unmarshal(out, &n))
	return n.ID
}

func (f *relayNodesFixture) admin(t *testing.T, method, path, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+relayNodesAdminToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, out
}

func (f *relayNodesFixture) probe(t *testing.T) {
	t.Helper()
	require.NoError(t, f.monitor.ProbeAll(t.Context()))
}

// credentials requests relay credentials, optionally for a region.
func (f *relayNodesFixture) credentials(t *testing.T, region string) (int, relay.Credentials) {
	t.Helper()
	path := "/v1/relay/credentials"
	if region != "" {
		path += "?region=" + region
	}
	resp, body := authedReq(t, http.MethodPost, f.srv.URL+path, f.apiKey, f.access, nil)
	var creds relay.Credentials
	if resp.StatusCode == http.StatusOK {
		require.NoError(t, json.Unmarshal(body, &creds))
	}
	return resp.StatusCode, creds
}

// fakeLifecycle serves a fixed GET /lifecycle document whose state can change.
type fakeLifecycle struct {
	srv   *httptest.Server
	state atomic.Value
	code  atomic.Int64
}

func newFakeLifecycle(t *testing.T, state string) *fakeLifecycle {
	t.Helper()
	f := &fakeLifecycle{}
	f.state.Store(state)
	f.code.Store(http.StatusOK)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(f.code.Load()))
		_, _ = fmt.Fprintf(w, `{"role":"relay","state":%q,"active_work":0,"details":{}}`, f.state.Load())
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLifecycle) url() string { return f.srv.URL + "/lifecycle" }

// startLifecycleRelay boots a real TURN relay on loopback with its lifecycle
// routes, the way `ggscale-server relay` serves them on RELAY_HEALTH_ADDR.
func startLifecycleRelay(t *testing.T, issuer *relay.Issuer) (turnURL string, lc *lifecycle.Controller, lifecycleURL string) {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := conn.LocalAddr().(*net.UDPAddr).Port
	require.NoError(t, conn.Close())
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP: "127.0.0.1",
		BindAddr: "127.0.0.1",
		BindPort: port,
		Issuer:   issuer,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	lc = lifecycle.New(lifecycle.Config{Role: "relay", Commit: "test", Token: relayNodesProbeToken, Work: srv})
	mux := http.NewServeMux()
	lc.Mount(mux)
	health := httptest.NewServer(mux)
	t.Cleanup(health.Close)
	lc.MarkReady()
	return "turn:" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + "?transport=udp", lc, health.URL + "/lifecycle"
}

// allocateThrough dials the TURN URL from a credential set and allocates.
func allocateThrough(t *testing.T, creds relay.Credentials) error {
	t.Helper()
	require.NotEmpty(t, creds.URLs)
	addr := strings.TrimSuffix(strings.TrimPrefix(creds.URLs[0], "turn:"), "?transport=udp")
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client, err := turn.NewClient(&turn.ClientConfig{
		TURNServerAddr: addr,
		Conn:           conn,
		Username:       creds.Username,
		Password:       creds.Password,
		Realm:          creds.Realm,
		LoggerFactory:  logging.NewDefaultLoggerFactory(),
	})
	require.NoError(t, err)
	t.Cleanup(client.Close)
	require.NoError(t, client.Listen())
	relayConn, err := client.Allocate()
	if err != nil {
		return err
	}
	return relayConn.Close()
}

func TestRelayNodes_should_allocate_through_a_node_registered_by_the_admin_api(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-e2e")
	turnURL, _, lifecycleURL := startLifecycleRelay(t, f.issuer)
	f.register(t, "us-east", lifecycleURL, turnURL)
	f.probe(t)

	code, creds := f.credentials(t, "us-east")
	require.Equal(t, http.StatusOK, code)

	assert.NoError(t, allocateThrough(t, creds))
}

func TestRelayNodes_should_list_the_node_and_its_region_in_credentials(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-region")
	turnURL, _, lifecycleURL := startLifecycleRelay(t, f.issuer)
	f.register(t, "us-east", lifecycleURL, turnURL)
	f.probe(t)

	_, creds := f.credentials(t, "us-east")

	assert.Equal(t, []any{[]string{turnURL}, "us-east"}, []any{creds.URLs, creds.Region})
}

func TestRelayNodes_should_return_503_with_no_eligible_node(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-none")

	code, _ := f.credentials(t, "")

	assert.Equal(t, http.StatusServiceUnavailable, code)
}

func TestRelayNodes_should_not_advertise_a_node_before_its_first_probe(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-unprobed")
	lc := newFakeLifecycle(t, "active")
	f.register(t, "us-east", lc.url(), "turn:a.example.com:3478")

	code, _ := f.credentials(t, "us-east")

	assert.Equal(t, http.StatusServiceUnavailable, code)
}

func TestRelayNodes_should_stop_advertising_a_draining_node_on_the_next_probe(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-drain")
	_, relayLC, lifecycleURL := startLifecycleRelay(t, f.issuer)
	f.register(t, "us-east", lifecycleURL, "turn:a.example.com:3478")
	f.probe(t)
	relayLC.Drain()

	f.probe(t)

	code, _ := f.credentials(t, "us-east")
	assert.Equal(t, http.StatusServiceUnavailable, code)
}

func TestRelayNodes_should_advertise_a_resumed_node_again(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-resume")
	_, relayLC, lifecycleURL := startLifecycleRelay(t, f.issuer)
	f.register(t, "us-east", lifecycleURL, "turn:a.example.com:3478")
	relayLC.Drain()
	f.probe(t)
	relayLC.Resume()

	f.probe(t)

	code, _ := f.credentials(t, "us-east")
	assert.Equal(t, http.StatusOK, code)
}

func TestRelayNodes_should_stop_advertising_a_node_whose_lifecycle_url_times_out(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-timeout")
	lc := newFakeLifecycle(t, "active")
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		lc.srv.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		close(release)
		slow.Close()
	})
	f.register(t, "us-east", slow.URL+"/lifecycle", "turn:a.example.com:3478")

	f.probe(t)

	code, _ := f.credentials(t, "us-east")
	assert.Equal(t, http.StatusServiceUnavailable, code)
}

func TestRelayNodes_should_mark_a_node_unhealthy_on_401(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-401")
	lc := newFakeLifecycle(t, "active")
	lc.code.Store(http.StatusUnauthorized)
	id := f.register(t, "us-east", lc.url(), "turn:a.example.com:3478")

	f.probe(t)

	nodes, err := f.store.List(t.Context())
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	assert.Equal(t, []any{id, false}, []any{nodes[0].ID, nodes[0].LastProbeOK})
}

func TestRelayNodes_should_stop_advertising_a_node_whose_last_probe_is_stale(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-stale")
	lc := newFakeLifecycle(t, "active")
	id := f.register(t, "us-east", lc.url(), "turn:a.example.com:3478")
	f.probe(t)
	_, err := f.c.bootstrapPool.Exec(context.Background(),
		`UPDATE relay_nodes SET last_probe_at = now() - interval '31 seconds' WHERE id = $1`, id)
	require.NoError(t, err)

	code, _ := f.credentials(t, "us-east")

	assert.Equal(t, http.StatusServiceUnavailable, code)
}

func TestRelayNodes_should_serve_only_the_requested_region_when_it_has_a_node(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-x")
	f.register(t, "us-east", newFakeLifecycle(t, "active").url(), "turn:us.example.com:3478")
	f.register(t, "eu-west", newFakeLifecycle(t, "active").url(), "turn:eu.example.com:3478")
	f.probe(t)

	_, creds := f.credentials(t, "eu-west")

	assert.Equal(t, []string{"turn:eu.example.com:3478"}, creds.URLs)
}

func TestRelayNodes_should_serve_every_eligible_node_for_an_unknown_region(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-unknown")
	f.register(t, "us-east", newFakeLifecycle(t, "active").url(), "turn:us.example.com:3478")
	f.register(t, "eu-west", newFakeLifecycle(t, "active").url(), "turn:eu.example.com:3478")
	f.probe(t)

	_, creds := f.credentials(t, "ap-south")

	assert.Equal(t, []any{2, ""}, []any{len(creds.URLs), creds.Region})
}

func TestRelayNodes_should_serve_every_eligible_node_without_a_region(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-empty")
	f.register(t, "us-east", newFakeLifecycle(t, "active").url(), "turn:us.example.com:3478")
	f.register(t, "eu-west", newFakeLifecycle(t, "active").url(), "turn:eu.example.com:3478")
	f.probe(t)

	_, creds := f.credentials(t, "")

	assert.ElementsMatch(t, []string{"turn:us.example.com:3478", "turn:eu.example.com:3478"}, creds.URLs)
}

func TestRelayNodes_should_shuffle_url_order_between_calls(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-shuffle")
	for _, host := range []string{"a", "b", "c"} {
		f.register(t, "us-east", newFakeLifecycle(t, "active").url(), "turn:"+host+".example.com:3478")
	}
	f.probe(t)
	orders := map[string]bool{}

	for range 8 {
		code, creds := f.credentials(t, "us-east")
		require.Equal(t, http.StatusOK, code)
		orders[strings.Join(creds.URLs, ",")] = true
	}

	assert.Greater(t, len(orders), 1)
}

func TestRelayNodes_should_never_advertise_or_probe_a_retired_node(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-retire")
	lc := newFakeLifecycle(t, "active")
	id := f.register(t, "us-east", lc.url(), "turn:a.example.com:3478")
	resp, body := f.admin(t, http.MethodPost, fmt.Sprintf("/v1/admin/relay-nodes/%d/retire", id), "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	f.probe(t)

	nodes, err := f.store.List(t.Context())
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	assert.Nil(t, nodes[0].LastProbeAt)
}

func TestRelayNodes_should_allow_reregistering_a_retired_lifecycle_url(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-rereg")
	lc := newFakeLifecycle(t, "active")
	id := f.register(t, "us-east", lc.url(), "turn:a.example.com:3478")
	f.admin(t, http.MethodPost, fmt.Sprintf("/v1/admin/relay-nodes/%d/retire", id), "")

	second := f.register(t, "us-east", lc.url(), "turn:a.example.com:3478")

	assert.NotEqual(t, id, second)
}

func TestRelayNodes_should_refuse_a_duplicate_known_lifecycle_url(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-dup")
	lc := newFakeLifecycle(t, "active")
	f.register(t, "us-east", lc.url(), "turn:a.example.com:3478")
	body := fmt.Sprintf(`{"region":"us-east","turn_urls":["turn:b.example.com:3478"],"lifecycle_url":%q}`, lc.url())

	resp, _ := f.admin(t, http.MethodPost, "/v1/admin/relay-nodes", body)

	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestRelayNodes_should_report_eligibility_in_the_admin_list(t *testing.T) {
	f := newRelayNodesFixture(t, "relay-nodes-list")
	f.register(t, "us-east", newFakeLifecycle(t, "active").url(), "turn:a.example.com:3478")
	f.probe(t)

	resp, body := f.admin(t, http.MethodGet, "/v1/admin/relay-nodes", "")

	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Nodes []relaynode.Node `json:"nodes"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	require.Len(t, out.Nodes, 1)
	assert.True(t, out.Nodes[0].Eligible)
}
