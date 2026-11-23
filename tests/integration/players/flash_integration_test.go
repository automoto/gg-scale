//go:build integration

package players_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every account page that shows a flash message must ignore free text in
// the URL.
func TestPlayerFlash_should_not_show_free_text_from_the_url(t *testing.T) {
	s := startSSOSite(t)
	createVerifiedAccount(t, s.raw, "flash@example.com")
	c, _ := s.passwordSession(t, "flash@example.com")
	query := "?" + url.Values{"flash": {"FLASHMARKER call 555-0100"}, "error": {"FLASHMARKER call 555-0100"}}.Encode()

	for _, page := range []string{"/", "/remote-addrs", "/friends"} {
		t.Run(page, func(t *testing.T) {
			resp, body := browserGet(t, c, s.url+accountBasePath+page+query)

			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.NotContains(t, body, "FLASHMARKER")
		})
	}
}

func TestPlayerFlash_should_show_the_text_of_a_known_code(t *testing.T) {
	s := startSSOSite(t)
	createVerifiedAccount(t, s.raw, "flash-code@example.com")
	c, _ := s.passwordSession(t, "flash-code@example.com")

	_, body := browserGet(t, c, s.url+accountBasePath+"/friends?flash=friend_self")

	assert.Contains(t, body, "You can&#39;t friend yourself.")
}
