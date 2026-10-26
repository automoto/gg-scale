package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/relaynode"
)

const platformAdminTestToken = "platform-admin-token-0123456789abcdef"

// fakeRelayNodeAdmin is an in-memory relay node registry.
type fakeRelayNodeAdmin struct {
	nodes []relaynode.Node
}

func (f *fakeRelayNodeAdmin) Create(_ context.Context, spec relaynode.Spec) (relaynode.Node, error) {
	if err := spec.Validate(); err != nil {
		return relaynode.Node{}, err
	}
	for _, n := range f.nodes {
		if n.State == relaynode.StateKnown && n.LifecycleURL == spec.LifecycleURL {
			return relaynode.Node{}, relaynode.ErrDuplicate
		}
	}
	n := relaynode.Node{
		ID: int64(len(f.nodes) + 1), Region: spec.Region, TURNURLs: spec.TURNURLs,
		LifecycleURL: spec.LifecycleURL, State: relaynode.StateKnown,
	}
	f.nodes = append(f.nodes, n)
	return n, nil
}

func (f *fakeRelayNodeAdmin) List(context.Context) ([]relaynode.Node, error) { return f.nodes, nil }

func (f *fakeRelayNodeAdmin) Retire(_ context.Context, id int64) (relaynode.Node, error) {
	for i := range f.nodes {
		if f.nodes[i].ID == id {
			f.nodes[i].State = relaynode.StateRetired
			return f.nodes[i], nil
		}
	}
	return relaynode.Node{}, relaynode.ErrNotFound
}

func newAdminRouter(token string) http.Handler {
	return httpapi.NewRouter(httpapi.Deps{
		Version: "v1", PlatformAdminToken: token, RelayNodeAdmin: &fakeRelayNodeAdmin{},
	})
}

func adminDo(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const validNodeBody = `{"region":"us-east","turn_urls":["turn:relay-1.example.com:3478?transport=udp"],"lifecycle_url":"http://10.0.0.5:9091/lifecycle"}`

func TestAdminRelayNodes_should_not_mount_without_a_token(t *testing.T) {
	h := newAdminRouter("")

	rec := adminDo(t, h, http.MethodGet, "/v1/admin/relay-nodes", platformAdminTestToken, "")

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAdminRelayNodes_should_refuse_a_wrong_token(t *testing.T) {
	h := newAdminRouter(platformAdminTestToken)

	rec := adminDo(t, h, http.MethodGet, "/v1/admin/relay-nodes", "wrong-token-0123456789abcdef0123456789", "")

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAdminRelayNodes_should_refuse_a_missing_token(t *testing.T) {
	h := newAdminRouter(platformAdminTestToken)

	rec := adminDo(t, h, http.MethodPost, "/v1/admin/relay-nodes", "", validNodeBody)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAdminRelayNodes_should_create_a_known_node(t *testing.T) {
	h := newAdminRouter(platformAdminTestToken)

	rec := adminDo(t, h, http.MethodPost, "/v1/admin/relay-nodes", platformAdminTestToken, validNodeBody)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var n relaynode.Node
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &n))
	assert.Equal(t, relaynode.StateKnown, n.State)
}

func TestAdminRelayNodes_should_reject_an_invalid_region(t *testing.T) {
	h := newAdminRouter(platformAdminTestToken)
	body := strings.Replace(validNodeBody, `"us-east"`, `"US East"`, 1)

	rec := adminDo(t, h, http.MethodPost, "/v1/admin/relay-nodes", platformAdminTestToken, body)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestAdminRelayNodes_should_reject_a_non_turn_url(t *testing.T) {
	h := newAdminRouter(platformAdminTestToken)
	body := strings.Replace(validNodeBody, "turn:relay-1", "https://relay-1", 1)

	rec := adminDo(t, h, http.MethodPost, "/v1/admin/relay-nodes", platformAdminTestToken, body)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestAdminRelayNodes_should_refuse_a_duplicate_lifecycle_url(t *testing.T) {
	h := newAdminRouter(platformAdminTestToken)
	adminDo(t, h, http.MethodPost, "/v1/admin/relay-nodes", platformAdminTestToken, validNodeBody)

	rec := adminDo(t, h, http.MethodPost, "/v1/admin/relay-nodes", platformAdminTestToken, validNodeBody)

	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestAdminRelayNodes_should_list_registered_nodes(t *testing.T) {
	h := newAdminRouter(platformAdminTestToken)
	adminDo(t, h, http.MethodPost, "/v1/admin/relay-nodes", platformAdminTestToken, validNodeBody)

	rec := adminDo(t, h, http.MethodGet, "/v1/admin/relay-nodes", platformAdminTestToken, "")

	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		Nodes []relaynode.Node `json:"nodes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Len(t, out.Nodes, 1)
}

func TestAdminRelayNodes_should_list_an_empty_registry_as_an_empty_array(t *testing.T) {
	h := newAdminRouter(platformAdminTestToken)

	rec := adminDo(t, h, http.MethodGet, "/v1/admin/relay-nodes", platformAdminTestToken, "")

	assert.JSONEq(t, `{"nodes":[]}`, rec.Body.String())
}

func TestAdminRelayNodes_should_retire_a_node(t *testing.T) {
	h := newAdminRouter(platformAdminTestToken)
	adminDo(t, h, http.MethodPost, "/v1/admin/relay-nodes", platformAdminTestToken, validNodeBody)

	rec := adminDo(t, h, http.MethodPost, "/v1/admin/relay-nodes/1/retire", platformAdminTestToken, "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var n relaynode.Node
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &n))
	assert.Equal(t, relaynode.StateRetired, n.State)
}

func TestAdminRelayNodes_should_return_404_when_retiring_an_unknown_node(t *testing.T) {
	h := newAdminRouter(platformAdminTestToken)

	rec := adminDo(t, h, http.MethodPost, "/v1/admin/relay-nodes/99/retire", platformAdminTestToken, "")

	assert.Equal(t, http.StatusNotFound, rec.Code)
}
