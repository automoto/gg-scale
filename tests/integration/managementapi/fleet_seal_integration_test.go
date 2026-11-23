//go:build integration

package managementapi_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/managementtoken"
	"github.com/automoto/gg-scale/internal/secretseal"
)

const fleetSealMarker = "tok-SEALMARKER-7f3a"

func fleetTestCipher(t *testing.T) *secretseal.Cipher {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	c, err := secretseal.NewCipherKeyring(key)
	require.NoError(t, err)
	return c
}

func (f *fixture) fleetConfigColumn(fleetID int64) string {
	f.t.Helper()
	var raw string
	require.NoError(f.t, f.owner.QueryRow(f.t.Context(), `SELECT config::text FROM fleets WHERE id = $1`, fleetID).Scan(&raw))
	return raw
}

func TestFleetWrite_should_not_open_a_sealed_value_put_in_an_agones_config(t *testing.T) {
	c := fleetTestCipher(t)
	f := newFleetFixtureWithCipher(t, c, fakeBackend{})
	tok := f.scopedToken(managementtoken.ScopeFleetRead, managementtoken.ScopeFleetWrite)
	sealed, err := c.Encrypt([]byte(fleetSealMarker))
	require.NoError(t, err)
	frame := "ggseal1:" + base64.StdEncoding.EncodeToString(sealed)

	created := f.do(tok, http.MethodPost, fleetsPath, `{"name":"a","backend":"agones","config":{"fleet_name":"`+frame+`"}}`)
	read := f.do(tok, http.MethodGet, fleetsPath+"/"+id(int64(created.body["id"].(float64))), "")

	assert.Equal(t, []any{200, frame, 200, frame, false, false},
		[]any{created.status, created.body["config"].(map[string]any)["fleet_name"],
			read.status, read.body["config"].(map[string]any)["fleet_name"],
			strings.Contains(created.raw, fleetSealMarker), strings.Contains(read.raw, fleetSealMarker)},
		created.raw+read.raw)
}

func TestFleetWrite_should_not_store_a_plain_plugin_value(t *testing.T) {
	f := newFleetFixtureWithCipher(t, fleetTestCipher(t), fakeBackend{})
	tok := f.scopedToken(managementtoken.ScopeFleetWrite)

	created := f.do(tok, http.MethodPost, fleetsPath, `{"name":"p","backend":"plugin:demo","config":{"api_token":"`+fleetSealMarker+`"}}`)
	fleetID := int64(created.body["id"].(float64))
	afterCreate := f.fleetConfigColumn(fleetID)
	updated := f.do(tok, http.MethodPut, fleetsPath+"/"+id(fleetID), `{"config":{"api_token":"`+fleetSealMarker+`-new"}}`)
	afterUpdate := f.fleetConfigColumn(fleetID)

	assert.Equal(t, []any{200, false, true, 200, false, true},
		[]any{created.status, strings.Contains(afterCreate, fleetSealMarker), sealedToken(t, afterCreate),
			updated.status, strings.Contains(afterUpdate, fleetSealMarker), sealedToken(t, afterUpdate)},
		afterCreate+afterUpdate)
}

// sealedToken reports whether a stored config has the api_token key with a
// sealed value, so an empty or dropped config does not pass.
func sealedToken(t *testing.T, raw string) bool {
	t.Helper()
	var cfg map[string]string
	require.NoError(t, json.Unmarshal([]byte(raw), &cfg))
	return strings.HasPrefix(cfg["api_token"], "ggseal1:")
}

// cleanerBackend is fakeBackend with resolution cleanup, so a deallocate
// reads the fleet config.
type cleanerBackend struct {
	fakeBackend
	called *bool
}

func (b cleanerBackend) Name() string { return "plugin:demo" }

func (b cleanerBackend) CleanupResolution(context.Context, int64, string, map[string]string) error {
	*b.called = true
	return nil
}

func TestAllocationWrite_deallocate_should_answer_422_when_a_plugin_value_does_not_open(t *testing.T) {
	called := false
	f := newFleetFixtureWithCipher(t, fleetTestCipher(t), cleanerBackend{called: &called})
	sealed, err := fleetTestCipher(t).Encrypt([]byte(fleetSealMarker))
	require.NoError(t, err)
	fleetID := f.scalar(`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		SELECT tenant_id, id, 'p', 'plugin:demo', jsonb_build_object('api_token', $2::text) FROM projects WHERE id = $1 RETURNING id`,
		f.projectA, "ggseal1:"+base64.StdEncoding.EncodeToString(sealed))
	allocID := f.scalar(`INSERT INTO game_server_allocations (tenant_id, project_id, fleet_id, backend, backend_ref, region, address, status, metadata)
		SELECT tenant_id, id, $2, 'plugin:demo', 'gs-1', 'eu', '203.0.113.5:7777', 'ready', '{"ggscale.dev/resolution-id":"mm_sealed"}'
		FROM projects WHERE id = $1 RETURNING id`, f.projectA, fleetID)
	tok := f.scopedToken(managementtoken.ScopeFleetRead, managementtoken.ScopeAllocationsWrite)

	got := f.do(tok, http.MethodPost, "/v1/project/allocations/"+id(allocID)+"/deallocate", "")

	assert.Equal(t, []any{http.StatusUnprocessableEntity, true, false},
		[]any{got.status, strings.Contains(got.detail(), "cannot be opened"), called}, got.raw)
}
