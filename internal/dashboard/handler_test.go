package dashboard

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ggscale/ggscale/internal/ratelimit"
)

func TestClientIP_ignoresForwardedHeaderByDefault(t *testing.T) {
	h := &Handler{}
	r := &http.Request{
		RemoteAddr: "203.0.113.10:4321",
		Header:     http.Header{},
	}
	r.Header.Set("CF-Connecting-IP", "198.51.100.44")

	got := h.clientIP(r)

	assert.Equal(t, "203.0.113.10", got)
}

func TestClientIP_honorsForwardedHeaderFromTrustedProxy(t *testing.T) {
	h := &Handler{
		proxyTrust: ratelimit.NewProxyTrust("CF-Connecting-IP", []string{"10.0.0.0/8"}),
	}
	r := &http.Request{
		RemoteAddr: "10.1.2.3:4321",
		Header:     http.Header{},
	}
	r.Header.Set("CF-Connecting-IP", "198.51.100.44")

	got := h.clientIP(r)

	assert.Equal(t, "198.51.100.44", got)
}

func TestClientIP_ignoresForwardedHeaderFromUntrustedPeer(t *testing.T) {
	h := &Handler{
		proxyTrust: ratelimit.NewProxyTrust("CF-Connecting-IP", []string{"10.0.0.0/8"}),
	}
	r := &http.Request{
		RemoteAddr: "203.0.113.10:4321",
		Header:     http.Header{},
	}
	r.Header.Set("CF-Connecting-IP", "198.51.100.44")

	got := h.clientIP(r)

	assert.Equal(t, "203.0.113.10", got)
}
