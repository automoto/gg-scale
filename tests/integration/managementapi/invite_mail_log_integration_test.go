//go:build integration

package managementapi_test

import (
	"net"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/mailer"
	_ "github.com/automoto/gg-scale/internal/mailer/smtp"
	"github.com/automoto/gg-scale/internal/managementtoken"
	"github.com/automoto/gg-scale/internal/ratelimit"
)

// rejectingSMTP is a plain-text SMTP server on a local port that refuses
// each recipient with a reply that repeats the address, as real servers
// do. It returns the address to dial.
func rejectingSMTP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				tp := textproto.NewConn(conn)
				_ = tp.PrintfLine("220 fake ESMTP")
				for {
					line, err := tp.ReadLine()
					if err != nil || strings.HasPrefix(line, "QUIT") {
						return
					}
					rcpt, isRcpt := strings.CutPrefix(line, "RCPT TO:")
					if isRcpt {
						_ = tp.PrintfLine("550 5.1.1 %s: Recipient address rejected", rcpt)
						continue
					}
					_ = tp.PrintfLine("250 ok")
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func TestPlayerInvite_mailer_error_log_should_hold_no_address(t *testing.T) {
	m, err := mailer.New("smtp", rejectingSMTP(t), "", "", "no-reply@example.test", "off")
	require.NoError(t, err)
	f := newFixtureWith(t, func(d *httpapi.Deps) {
		d.Mailer = m
		d.MailFrom = "no-reply@example.test"
		d.ControlPanel.BaseURL = "http://app.example.test"
		d.EmailVerifySigningKey = []byte("0123456789abcdef0123456789abcdef")
		d.RateLimitOverrides = ratelimit.NewDBOverrideStore(d.Pool)
	})
	logs := captureLogs(t)
	tok := f.scopedToken(managementtoken.ScopePlayersInvite, managementtoken.ScopePlayersRead, managementtoken.ScopePlayersPII)

	got := f.do(tok, http.MethodPost, invitePath, inviteBody)

	out := logs.String()
	require.Equal(t, http.StatusOK, got.status, got.raw)
	assert.Equal(t, []bool{true, true, true, false}, []bool{
		strings.Contains(out, "player invite mailer"), strings.Contains(out, "RCPT: server replied 550"),
		strings.Contains(out, "domain=example.com"), strings.Contains(out, "newbie@"),
	}, out)
}
