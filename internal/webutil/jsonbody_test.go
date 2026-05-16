package webutil_test

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/webutil"
)

type body struct {
	Name string `json:"name"`
}

func TestDecodeJSONHappyPath(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"alice"}`))
	w := httptest.NewRecorder()

	got, err := webutil.DecodeJSON[body](w, r, 1<<20)
	require.NoError(t, err)
	assert.Equal(t, "alice", got.Name)
}

func TestDecodeJSONRejectsUnknownFields(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"alice","oops":1}`))
	w := httptest.NewRecorder()

	_, err := webutil.DecodeJSON[body](w, r, 1<<20)
	assert.Error(t, err)
}

func TestDecodeJSONRejectsTrailingJunk(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"a"} {"name":"b"}`))
	w := httptest.NewRecorder()

	_, err := webutil.DecodeJSON[body](w, r, 1<<20)
	assert.Error(t, err)
}

func TestDecodeJSONWrapsMaxBytesError(t *testing.T) {
	big := strings.Repeat("a", 100)
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"`+big+`"}`))
	w := httptest.NewRecorder()

	_, err := webutil.DecodeJSON[body](w, r, 32)
	assert.True(t, errors.Is(err, webutil.ErrBodyTooLarge), "wanted ErrBodyTooLarge, got %v", err)
}
