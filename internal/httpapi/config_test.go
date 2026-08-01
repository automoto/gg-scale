package httpapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRemoteConfigETag_isStableForCanonicalConfig(t *testing.T) {
	config := []byte(`{"enabled": true, "minimum_client_version": "1.4.0"}`)

	first := remoteConfigETag(config)
	second := remoteConfigETag(config)

	assert.Equal(t, first, second)
}

func TestRemoteConfigETag_changesWithConfig(t *testing.T) {
	before := remoteConfigETag([]byte(`{"enabled": true}`))
	after := remoteConfigETag([]byte(`{"enabled": false}`))

	assert.NotEqual(t, before, after)
}

func TestETagMatches_acceptsWeakAndStrongValidators(t *testing.T) {
	etag := `"remote-config-abc123"`

	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"strong", etag, true},
		{"weak", "W/" + etag, true},
		{"list", `"other", W/` + etag, true},
		{"wildcard", "*", true},
		{"mismatch", `"other"`, false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, etagMatches(tc.header, etag))
		})
	}
}
