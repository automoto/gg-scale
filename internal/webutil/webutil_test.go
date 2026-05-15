package webutil

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecurityHeaders_sets_expected_headers(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := SecurityHeaders(next)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "same-origin", rec.Header().Get("Referrer-Policy"))
}

func TestParseForm_returns_true_on_valid_form(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("k=v"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	assert.True(t, ParseForm(rec, req))
	assert.Equal(t, "v", req.Form.Get("k"))
}

func TestParseForm_is_idempotent_when_already_parsed(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("k=v"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	require.NoError(t, req.ParseForm())
	rec := httptest.NewRecorder()
	assert.True(t, ParseForm(rec, req))
}

func TestIsUniqueViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"typed_23505", &pgconn.PgError{Code: "23505"}, true},
		{"typed_other_code", &pgconn.PgError{Code: "42P01"}, false},
		{"wrapped_typed_23505", &wrappedErr{inner: &pgconn.PgError{Code: "23505"}}, true},
		{"substring_23505", errors.New("ERROR: duplicate key (SQLSTATE 23505)"), true},
		{"plain_error", errors.New("oops"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsUniqueViolation(tc.err))
		})
	}
}

type wrappedErr struct{ inner error }

func (w *wrappedErr) Error() string { return w.inner.Error() }
func (w *wrappedErr) Unwrap() error { return w.inner }

func TestRandomHex_length_and_prefix(t *testing.T) {
	s, err := RandomHex("pfx_", 16)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(s, "pfx_"))
	assert.Len(t, s, len("pfx_")+32) // 16 bytes → 32 hex chars
}

func TestRandomHex_returns_distinct_values(t *testing.T) {
	seen := make(map[string]struct{}, 20)
	for i := 0; i < 20; i++ {
		s, err := RandomHex("", 8)
		require.NoError(t, err)
		seen[s] = struct{}{}
	}
	assert.Len(t, seen, 20)
}
