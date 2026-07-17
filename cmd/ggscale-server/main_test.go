package main

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/ggscale/ggscale/internal/config"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMigrateArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    migrateCmd
		wantErr bool
	}{
		{"version", []string{"version"}, migrateCmd{action: "version"}, false},
		{"force with version", []string{"force", "7"}, migrateCmd{action: "force", version: 7}, false},
		{"force version zero", []string{"force", "0"}, migrateCmd{action: "force", version: 0}, false},
		{"no args", nil, migrateCmd{}, true},
		{"unknown subcommand", []string{"bogus"}, migrateCmd{}, true},
		{"force without version", []string{"force"}, migrateCmd{}, true},
		{"force non-numeric", []string{"force", "abc"}, migrateCmd{}, true},
		{"force negative", []string{"force", "-1"}, migrateCmd{}, true},
		{"force extra args", []string{"force", "7", "8"}, migrateCmd{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMigrateArgs(tt.args)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMigrationURLUsesElevatedCredentialWhenConfigured(t *testing.T) {
	tests := []struct {
		name        string
		databaseURL string
		migrateURL  string
		want        string
	}{
		{
			name:        "development fallback",
			databaseURL: "postgres://app@db/ggscale",
			want:        "postgres://app@db/ggscale",
		},
		{
			name:        "separate migration credential",
			databaseURL: "postgres://app@db/ggscale",
			migrateURL:  "postgres://owner@db/ggscale",
			want:        "postgres://owner@db/ggscale",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				DatabaseURL:  tt.databaseURL,
				DBMigrateURL: tt.migrateURL,
			}

			assert.Equal(t, tt.want, migrationURL(cfg))
		})
	}
}

func TestServer_listens_and_responds_to_v1_healthz(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := &http.Server{
		Handler:           httpapi.NewRouter(httpapi.Deps{Version: "v1", Commit: "test"}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	url := "http://" + ln.Addr().String() + "/v1/healthz"
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"status":"ok"`)
}
