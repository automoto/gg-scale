//go:build integration

package managementapi_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/secretseal"
)

const (
	steamTestKey  = "0123456789abcdef0123456789ABCDEF"
	steamAuditSQL = `SELECT payload::text FROM platform_audit_log WHERE action = 'control_panel.steam_auth.update'
		AND actor_service = $1 AND target = $2`
)

// withCipher seals stored provider credentials, as a real server does.
func withCipher(d *httpapi.Deps) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(200 + i%50)
	}
	c, err := secretseal.NewCipherKeyring(key)
	if err != nil {
		panic(err)
	}
	d.CredentialCipher = c
}

func (f *fixture) steamRow() (string, []byte) {
	f.t.Helper()
	var appID string
	var key []byte
	require.NoError(f.t, f.owner.QueryRow(context.Background(),
		`SELECT steam_app_id, steam_web_api_key FROM projects WHERE id = $1`, f.projectA).Scan(&appID, &key))
	return appID, key
}

func steamSetBody(appID, key string) string {
	if key == "" {
		return `{"app_id":"` + appID + `"}`
	}
	return `{"app_id":"` + appID + `","web_api_key":"` + key + `"}`
}

func TestSteamWrite_should_return_403_without_the_steam_write_scope(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("config:read", "config:write")

	set := f.do(tok, http.MethodPut, steamPath, steamSetBody("480", steamTestKey))
	clear := f.do(tok, http.MethodDelete, steamPath, "")

	appID, key := f.steamRow()
	assert.Equal(t, []any{403, 403, "", 0}, []any{set.status, clear.status, appID, len(key)}, set.raw+clear.raw)
}

func TestSteamWrite_should_return_403_when_the_creator_lacks_the_config_update_pair(t *testing.T) {
	f := newFixture(t)
	admin := f.user("admin@example.com", f.tenantA, "admin")
	tok, _ := f.token(f.tenantA, f.projectA, admin, []string{"steam:write"}, time.Hour)
	f.dropPair("role:tenant_admin", "project:*:config", "update")

	set := f.do(tok, http.MethodPut, steamPath, steamSetBody("480", steamTestKey))

	appID, _ := f.steamRow()
	assert.Equal(t, []any{403, ""}, []any{set.status, appID}, set.raw)
}

func TestSteamWrite_should_seal_the_key_and_audit_as_the_token(t *testing.T) {
	f := newFixtureWith(t, withCipher)
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{"steam:write"}, time.Hour)

	got := f.do(tok, http.MethodPut, steamPath, steamSetBody("480", steamTestKey))

	appID, key := f.steamRow()
	var audit string
	require.NoError(t, f.owner.QueryRow(context.Background(), steamAuditSQL,
		"management_token:"+strconv.FormatInt(tokenID, 10), strconv.FormatInt(f.projectA, 10)).Scan(&audit))
	assert.Equal(t, []any{200, "480", true, "480", true}, []any{got.status, got.body["app_id"], got.body["web_api_key_set"], appID, len(key) > 0}, got.raw)
	assert.NotContains(t, string(key), steamTestKey, "the stored key must be sealed")
	assert.Contains(t, audit, `"key_changed": true`)
}

func TestSteamWrite_should_keep_the_key_when_only_the_app_id_is_given(t *testing.T) {
	f := newFixtureWith(t, withCipher)
	tok := f.scopedToken("steam:write")
	require.Equal(t, 200, f.do(tok, http.MethodPut, steamPath, steamSetBody("480", steamTestKey)).status)
	_, before := f.steamRow()

	got := f.do(tok, http.MethodPut, steamPath, steamSetBody("730", ""))

	appID, after := f.steamRow()
	assert.Equal(t, []any{200, "730", before}, []any{got.status, appID, after}, got.raw)
}

func TestSteamWrite_should_return_422_with_the_dashboard_message_for_invalid_input(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("steam:write")
	cases := map[string]struct{ body, msg string }{
		"app id":      {steamSetBody("not-a-number", steamTestKey), "Enter the numeric Steam App ID."},
		"short key":   {steamSetBody("480", "abc123"), "Enter the 32-character publisher Web API key."},
		"non hex key": {steamSetBody("480", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"), "Enter the 32-character publisher Web API key."},
		"no key set":  {steamSetBody("480", ""), "Enter your publisher Web API key to enable Steam sign-in."},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := f.do(tok, http.MethodPut, steamPath, c.body)

			appID, key := f.steamRow()
			assert.Equal(t, []any{422, c.msg, "", 0}, []any{got.status, got.detail(), appID, len(key)}, got.raw)
		})
	}
}

func TestSteamWrite_should_clear_the_app_id_and_key_and_audit_it(t *testing.T) {
	f := newFixture(t)
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{"steam:write"}, time.Hour)
	require.Equal(t, 200, f.do(tok, http.MethodPut, steamPath, steamSetBody("480", steamTestKey)).status)

	got := f.do(tok, http.MethodDelete, steamPath, "")

	appID, key := f.steamRow()
	rows := f.count(`SELECT count(*) FROM platform_audit_log WHERE action = 'control_panel.steam_auth.update'
		AND actor_service = $1 AND payload->>'cleared' = 'true'`, "management_token:"+strconv.FormatInt(tokenID, 10))
	assert.Equal(t, []any{200, nil, false, "", 0, 1},
		[]any{got.status, got.body["app_id"], got.body["web_api_key_set"], appID, len(key), rows}, got.raw)
}

// The key must be in no response, error, audit row, or log line, in any
// form. Sealed bytes are binary with non-ASCII bytes; the plain key is
// checked as text, hex, and base64.
func TestSteamWrite_should_keep_the_key_out_of_every_output(t *testing.T) {
	f := newFixtureWith(t, withCipher)
	logs := captureLogs(t)
	slog.Info("steam log capture is on")
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{"steam:write", "config:read"}, time.Hour)
	outputs := []string{
		f.do(tok, http.MethodPut, steamPath, steamSetBody("480", steamTestKey)).raw,
		f.do(tok, http.MethodGet, steamPath, "").raw,
		f.do(tok, http.MethodPut, steamPath, steamSetBody("480x", steamTestKey)).raw,
		f.do(tok, http.MethodPut, steamPath, `{"app_id":"480","web_api_key":"`+steamTestKey+`","extra":1}`).raw,
		f.do(tok, http.MethodPut, steamPath, `{"app_id":480,"web_api_key":"`+steamTestKey+`"}`).raw,
		f.do(tok, http.MethodPut, steamPath, `{"app_id":"480","web_api_key":"`+steamTestKey+`"`).raw,
		resultText(f.call(tok, "set_steam_auth", map[string]any{"app_id": 480, "web_api_key": steamTestKey})),
		f.rpcError(tok, "set_steam_auth", map[string]any{"app_id": "480", "web_api_key": steamTestKey, "extra": 1}),
		resultText(f.call(tok, "set_steam_auth", map[string]any{"app_id": "480", "web_api_key": steamTestKey})),
		resultText(f.call(tok, "get_steam_auth", map[string]any{})),
		f.do(tok, http.MethodGet, "/v1/project/health", "").raw,
	}
	_, sealed := f.steamRow()
	var audit string
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT string_agg(payload::text, ' ') FROM platform_audit_log WHERE action = 'control_panel.steam_auth.update'
		AND actor_service = $1`, "management_token:"+strconv.FormatInt(tokenID, 10)).Scan(&audit))
	outputs = append(outputs, audit, logs.String())
	require.NotEmpty(t, sealed)
	require.Contains(t, logs.String(), "steam log capture is on")

	forms := map[string]string{
		"plain": steamTestKey, "lower": "0123456789abcdef0123456789abcdef",
		"hex": hex.EncodeToString([]byte(steamTestKey)), "base64": base64.StdEncoding.EncodeToString([]byte(steamTestKey)),
		"sealed raw": string(sealed), "sealed hex": hex.EncodeToString(sealed),
		"sealed base64": base64.StdEncoding.EncodeToString(sealed), "sealed base64url": base64.RawURLEncoding.EncodeToString(sealed),
	}
	leaked := []string{}
	for name, form := range forms {
		for i, out := range outputs {
			if strings.Contains(out, form) {
				leaked = append(leaked, name+"@"+strconv.Itoa(i))
			}
		}
	}
	assert.Empty(t, leaked)
}

func TestSteamWrite_tools_should_set_and_clear_and_return_the_read_shape(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("steam:write")

	set := structured(f.call(tok, "set_steam_auth", map[string]any{"app_id": "480", "web_api_key": steamTestKey}))
	cleared := structured(f.call(tok, "clear_steam_auth", map[string]any{}))

	assert.Equal(t, []any{"480", true, nil, false}, []any{set["app_id"], set["web_api_key_set"], cleared["app_id"], cleared["web_api_key_set"]})
}
