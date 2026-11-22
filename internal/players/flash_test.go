package players

import (
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFlashFromRequest(t *testing.T) {
	tests := map[string]struct {
		query   string
		wantMsg string
		wantErr string
	}{
		"should_show_nothing_for_free_text":       {"?flash=Your+account+is+locked.", "", ""},
		"should_show_nothing_for_free_error_text": {"?error=Your+account+is+locked.", "", ""},
		"should_show_nothing_without_a_code":      {"", "", ""},
		"should_show_a_success_code_as_a_message": {"?flash=" + string(flashFriendAdded), "Friend added.", ""},
		"should_show_an_error_code_as_an_error":   {"?flash=" + string(flashFriendSelf), "", "You can't friend yourself."},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest("GET", accountBasePath+"/"+tc.query, nil)

			msg, errMsg := flashFromRequest(r)

			assert.Equal(t, []string{tc.wantMsg, tc.wantErr}, []string{msg, errMsg})
		})
	}
}

func TestFlashNotices_should_map_plain_codes_to_text(t *testing.T) {
	code := regexp.MustCompile(`^[a-z0-9_]+$`)
	var bad []string
	for k, v := range flashNotices {
		if !code.MatchString(string(k)) || v.text == "" {
			bad = append(bad, string(k))
		}
	}

	assert.Empty(t, bad)
}

// Only flash.go may read a page message from the URL, so a new page cannot
// show free text from a link.
func TestPackage_should_read_url_messages_only_in_flash_go(t *testing.T) {
	files, err := os.ReadDir(".")
	assert.NoError(t, err)
	var readers []string
	for _, f := range files {
		name := f.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "flash.go" {
			continue
		}
		src, err := os.ReadFile(name)
		assert.NoError(t, err)
		for _, param := range []string{`"flash"`, `"created"`, `"error"`, `"notice"`, `"message"`} {
			if strings.Contains(string(src), "Query().Get("+param+")") {
				readers = append(readers, name+" "+param)
			}
		}
	}

	assert.Empty(t, readers)
}
