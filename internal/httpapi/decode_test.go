package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDecodeJSON_rejects_body_over_one_megabyte(t *testing.T) {
	huge := strings.Repeat("x", 2<<20)
	body := bytes.NewBufferString(`{"data":"` + huge + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/", body)
	rec := httptest.NewRecorder()

	var into struct {
		Data string `json:"data"`
	}
	ok := decodeJSON(rec, req, &into)

	assert.False(t, ok)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestDecodeJSON_accepts_small_body(t *testing.T) {
	body := bytes.NewBufferString(`{"name":"alice"}`)
	req := httptest.NewRequest(http.MethodPost, "/", body)
	rec := httptest.NewRecorder()

	var into struct {
		Name string `json:"name"`
	}
	ok := decodeJSON(rec, req, &into)

	assert.True(t, ok)
	assert.Equal(t, "alice", into.Name)
}

func TestDecodeJSON_rejects_unknown_fields(t *testing.T) {
	body := bytes.NewBufferString(`{"name":"alice","extra":1}`)
	req := httptest.NewRequest(http.MethodPost, "/", body)
	rec := httptest.NewRecorder()

	var into struct {
		Name string `json:"name"`
	}
	ok := decodeJSON(rec, req, &into)

	assert.False(t, ok)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
