package controlpanel

import (
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/projectadmin"
)

func TestFlashMessage(t *testing.T) {
	tests := map[string]struct {
		query string
		want  string
	}{
		"should_show_nothing_for_free_text":    {"?flash=Your+account+is+locked.+Call+555-0100.", ""},
		"should_show_nothing_for_created_text": {"?created=Game+Project+%22evil%22+created.", ""},
		"should_show_nothing_without_a_code":   {"", ""},
		"should_show_the_text_of_a_known_code": {"?flash=" + string(flashInviteSent), "Invite sent."},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/control-panel/x"+tc.query, nil)

			got := flashMessage(r)

			assert.Equal(t, tc.want, got)
		})
	}
}

func TestFlashMessages_should_map_plain_codes_to_text(t *testing.T) {
	code := regexp.MustCompile(`^[a-z0-9_]+$`)
	var bad []string
	for k, v := range flashMessages {
		if !code.MatchString(string(k)) || v == "" {
			bad = append(bad, string(k))
		}
	}

	assert.Empty(t, bad)
}

// limitErrors lists the LimitError constants of
// internal/projectadmin/limits.go.
var limitErrors = []projectadmin.LimitError{
	projectadmin.ErrInvalidStorageMB, projectadmin.ErrStorageAboveTenant,
	projectadmin.ErrInvalidInviteLimit, projectadmin.ErrInviteAboveCap,
}

func TestLimitFlash_should_keep_the_limit_error_text(t *testing.T) {
	for _, e := range limitErrors {
		assert.Equal(t, e.Error(), flashMessages[limitFlash(e)])
	}
}

func TestLimitFlashes_should_have_an_entry_for_each_limit_error(t *testing.T) {
	var missing []string
	for _, e := range limitErrors {
		if _, ok := limitFlashes[e]; !ok {
			missing = append(missing, e.Error())
		}
	}

	assert.Empty(t, missing)
}

// A new LimitError constant in limits.go must be added to limitErrors and to
// limitFlashes.
func TestLimitErrors_should_list_every_constant_in_limits_go(t *testing.T) {
	src, err := os.ReadFile("../projectadmin/limits.go")
	assert.NoError(t, err)

	assert.Equal(t, strings.Count(string(src), "LimitError = \""), len(limitErrors))
}

// Only flash.go may read a page message from the URL or a url.Values, so a
// new page cannot show free text from a link. The patterns catch .Get,
// FormValue, a map index on Query(), and a constant or variable that holds
// the parameter name.
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
			for _, pattern := range []string{".Get(" + param + ")", "FormValue(" + param + ")", "Query()[" + param + "]", "= " + param} {
				if strings.Contains(string(src), pattern) {
					readers = append(readers, name+" "+pattern)
				}
			}
		}
	}

	assert.Empty(t, readers)
}
