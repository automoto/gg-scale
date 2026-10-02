package projectadmin

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeOrigin_should_accept_valid_origins(t *testing.T) {
	cases := map[string]string{
		"http://localhost:5173":          "http://localhost:5173",
		"https://html-classic.itch.zone": "https://html-classic.itch.zone",
		"HTTPS://Game.Example.COM":       "https://game.example.com",
		"https://game.example.com/":      "https://game.example.com",
		"https://game.example.com:443":   "https://game.example.com",
		"http://game.example.com:80":     "http://game.example.com",
		"https://game.example.com:8443":  "https://game.example.com:8443",
		"  https://game.example.com  ":   "https://game.example.com",
		"http://127.0.0.1:8080":          "http://127.0.0.1:8080",
		"http://[::1]:3000":              "http://[::1]:3000",
	}
	for in, want := range cases {
		got, err := NormalizeOrigin(in)

		assert.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
}

func TestNormalizeOrigin_should_refuse_invalid_origins(t *testing.T) {
	for _, in := range []string{
		"",
		"*",
		"https://*.example.com",
		"game.example.com",
		"ftp://game.example.com",
		"https://game.example.com/play",
		"https://game.example.com?x=1",
		"https://game.example.com#top",
		"https://user:pw@game.example.com",
		"https://game.example.com:0",
		"https://game.example.com:99999",
		"https://game.example.com:abc",
		"https://",
		"null",
		"https://game example.com",
	} {
		_, err := NormalizeOrigin(in)

		assert.Error(t, err, in)
	}
}

func TestNormalizeOrigins_should_remove_duplicates_after_normalizing(t *testing.T) {
	got, err := NormalizeOrigins([]string{"https://a.example", "HTTPS://A.EXAMPLE/", "", "http://localhost:5173"}, 20)

	assert.NoError(t, err)
	assert.Equal(t, []string{"https://a.example", "http://localhost:5173"}, got)
}

func TestNormalizeOrigins_should_refuse_list_above_maximum(t *testing.T) {
	_, err := NormalizeOrigins([]string{"https://a.example", "https://b.example", "https://c.example"}, 2)

	assert.ErrorContains(t, err, "at most 2")
}
