package steamauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAppID  = "480"
	testKey    = "0123456789ABCDEF0123456789ABCDEF"
	testTicket = "14000000048bcd42"
)

func fakeValve(t *testing.T, status int, body string) (*Client, *url.Values) {
	t.Helper()
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/ISteamUserAuth/AuthenticateUserTicket/v1/", r.URL.Path)
		gotQuery = r.URL.Query()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL}, &gotQuery
}

func okBody(steamID string, vac, publisher bool) string {
	vacStr, pubStr := "false", "false"
	if vac {
		vacStr = "true"
	}
	if publisher {
		pubStr = "true"
	}
	return `{"response":{"params":{"result":"OK","steamid":"` + steamID +
		`","ownersteamid":"76561197960265729","vacbanned":` + vacStr +
		`,"publisherbanned":` + pubStr + `}}}`
}

func TestVerify_ok_parses_result_and_sends_expected_query(t *testing.T) {
	c, query := fakeValve(t, http.StatusOK, okBody("76561197960265728", false, false))

	res, err := c.Verify(context.Background(), testAppID, testKey, testTicket)

	require.NoError(t, err)
	assert.Equal(t, "76561197960265728", res.SteamID)
	assert.Equal(t, "76561197960265729", res.OwnerSteamID)
	assert.False(t, res.VACBanned)
	assert.False(t, res.PublisherBanned)
	assert.Equal(t, testKey, query.Get("key"))
	assert.Equal(t, testAppID, query.Get("appid"))
	assert.Equal(t, testTicket, query.Get("ticket"))
	assert.Equal(t, WebAPIIdentity, query.Get("identity"))
}

func TestVerify_parses_ban_flags(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantVAC       bool
		wantPublisher bool
	}{
		{"vac_banned", okBody("76561197960265728", true, false), true, false},
		{"publisher_banned", okBody("76561197960265728", false, true), false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := fakeValve(t, http.StatusOK, tt.body)
			res, err := c.Verify(context.Background(), testAppID, testKey, testTicket)
			require.NoError(t, err)
			assert.Equal(t, tt.wantVAC, res.VACBanned)
			assert.Equal(t, tt.wantPublisher, res.PublisherBanned)
		})
	}
}

func TestVerify_error_body_is_invalid_ticket(t *testing.T) {
	c, _ := fakeValve(t, http.StatusOK,
		`{"response":{"error":{"errorcode":101,"errordesc":"Invalid ticket"}}}`)

	_, err := c.Verify(context.Background(), testAppID, testKey, testTicket)

	assert.ErrorIs(t, err, ErrInvalidTicket)
}

func TestVerify_upstream_failures_are_unavailable(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"http_403_bad_key", http.StatusForbidden, "Forbidden"},
		{"http_500", http.StatusInternalServerError, "boom"},
		{"malformed_json", http.StatusOK, "{nope"},
		{"missing_steamid", http.StatusOK, `{"response":{"params":{"result":"OK"}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := fakeValve(t, tt.status, tt.body)
			_, err := c.Verify(context.Background(), testAppID, testKey, testTicket)
			assert.ErrorIs(t, err, ErrUnavailable)
		})
	}
}

func TestVerify_connection_refused_is_unavailable_and_never_leaks_the_key(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // connection refused from now on
	c := &Client{BaseURL: srv.URL}

	_, err := c.Verify(context.Background(), testAppID, testKey, testTicket)

	require.ErrorIs(t, err, ErrUnavailable)
	assert.NotContains(t, err.Error(), testKey,
		"transport errors stringify the full URL; the key must be scrubbed")
}

func TestVerify_timeout_is_unavailable_and_never_leaks_the_key(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}}

	_, err := c.Verify(context.Background(), testAppID, testKey, testTicket)

	require.ErrorIs(t, err, ErrUnavailable)
	assert.NotContains(t, err.Error(), testKey)
}
