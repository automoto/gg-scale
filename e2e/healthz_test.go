//go:build e2e

package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHealthz_lite_stack_endpoints_respond(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"ggscale-server", "http://localhost:8080/v1/healthz"},
		{"prometheus", "http://localhost:9090/-/ready"},
		{"mailhog", "http://localhost:8025/"},
		{"dashboard", "http://localhost:3001/v1/dashboard/login"},
	}

	client := &http.Client{Timeout: 5 * time.Second}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := client.Get(c.url)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode)
		})
	}
}

func TestHealthz_v1_returns_version_header(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get("http://localhost:8080/v1/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "v1", resp.Header.Get("X-API-Version"))
}
