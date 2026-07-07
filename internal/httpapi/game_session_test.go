package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateXUID(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		valid bool
	}{
		{"empty", "", false},
		{"simple", "1234567890", true},
		{"max length", strings.Repeat("a", 64), true},
		{"too long", strings.Repeat("a", 65), false},
		{"control char", "abc\x00def", false},
		{"unicode ok", "プレイヤー", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.valid, validateXUID(tt.in))
		})
	}
}

// The 1..32-rune bound is enforced by the presence schema (huma counts runes).
// These are the rejection cases (→ 422). The accepted 32-rune multibyte case —
// the one that proves runes, not bytes — is asserted end-to-end in the
// integration suite (TestPresence_accepts_custom_status_rejects_empty), where a
// real DB backs the write.
func TestPresenceStatus_schema_rejects_out_of_range(t *testing.T) {
	cases := map[string]string{
		"empty":        "",
		"33 ascii":     strings.Repeat("a", 33),
		"33 multibyte": strings.Repeat("あ", 33), // 99 bytes, 33 runes
	}
	for name, status := range cases {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"status": status})
			assert.NoError(t, err)
			rec := postEnvelope(t, registerPresence, http.MethodPut, "/v1/presence", string(body))
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
		})
	}
}
