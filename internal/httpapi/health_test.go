package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHealthz_returns_status_version_commit(t *testing.T) {
	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Version: "v1",
		Commit:  "abc123",
	}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var payload map[string]string
	require.NoError(t, json.Unmarshal(body, &payload))

	cases := []struct {
		field string
		want  string
	}{
		{"status", "ok"},
		{"version", "v1"},
		{"commit", "abc123"},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			assert.Equal(t, c.want, payload[c.field])
		})
	}
}
