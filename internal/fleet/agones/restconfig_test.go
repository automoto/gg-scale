package agones

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRestConfigFromEnvVars(t *testing.T) {
	caPEM := "-----BEGIN CERTIFICATE-----\nMIIBkTCB+w==\n-----END CERTIFICATE-----\n"
	validCAB64 := base64.StdEncoding.EncodeToString([]byte(caPEM))

	cases := []struct {
		name      string
		apiURL    string
		saToken   string
		caCertB64 string
		wantErr   string
	}{
		{"happy path", "https://k3s.example.com:6443", "sa-token-abc", validCAB64, ""},
		{"missing api url", "", "sa-token-abc", validCAB64, "K3S_API_URL is required"},
		{"missing token", "https://k3s.example.com:6443", "", validCAB64, "K3S_SA_TOKEN is required"},
		{"missing ca", "https://k3s.example.com:6443", "sa-token-abc", "", "K3S_CA_CERT_B64 is required"},
		{"malformed url", "not a url", "sa-token-abc", validCAB64, "not a valid URL"},
		{"invalid base64", "https://k3s.example.com:6443", "sa-token-abc", "!!!not-base64!!!", "base64 decode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := restConfigFromEnvVars(tc.apiURL, tc.saToken, tc.caCertB64)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.apiURL, cfg.Host)
			assert.Equal(t, tc.saToken, cfg.BearerToken)
			assert.Equal(t, []byte(caPEM), cfg.CAData)
		})
	}
}
