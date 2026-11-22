//go:build integration

package managementapi_test

import (
	"crypto/rand"
	"encoding/base64"
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
	f := newFleetFixtureWithCipher(t, c)
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
	f := newFleetFixtureWithCipher(t, fleetTestCipher(t))
	tok := f.scopedToken(managementtoken.ScopeFleetWrite)

	created := f.do(tok, http.MethodPost, fleetsPath, `{"name":"p","backend":"plugin:demo","config":{"api_token":"`+fleetSealMarker+`"}}`)
	fleetID := int64(created.body["id"].(float64))
	afterCreate := f.fleetConfigColumn(fleetID)
	updated := f.do(tok, http.MethodPut, fleetsPath+"/"+id(fleetID), `{"config":{"api_token":"`+fleetSealMarker+`-new"}}`)
	afterUpdate := f.fleetConfigColumn(fleetID)

	assert.Equal(t, []any{200, false, 200, false},
		[]any{created.status, strings.Contains(afterCreate, fleetSealMarker), updated.status, strings.Contains(afterUpdate, fleetSealMarker)},
		afterCreate+afterUpdate)
}
