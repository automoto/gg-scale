package smtp

import (
	"context"
	"net"
	"net/textproto"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/mailer"
)

func TestBuildRFC5322RejectsHeaderInjection(t *testing.T) {
	_, err := buildRFC5322(mailer.Message{
		From:    "no-reply@example.test",
		To:      []string{"player@example.test"},
		Subject: "welcome\r\nBcc: attacker@example.test",
		Body:    "hello",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "subject header")
}

func TestBuildRFC5322WritesListUnsubscribeHeaders(t *testing.T) {
	body, err := buildRFC5322(mailer.Message{
		From:            "no-reply@example.test",
		To:              []string{"player@example.test"},
		Subject:         "Invite",
		Body:            "hello",
		ListUnsubscribe: "https://app.example.test/v1/unsubscribe?token=abc",
	})

	require.NoError(t, err)
	msg := string(body)
	assert.Contains(t, msg, "List-Unsubscribe: <https://app.example.test/v1/unsubscribe?token=abc>\r\n")
	assert.Contains(t, msg, "List-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n")
}

func TestBuildRFC5322OmitsListUnsubscribeWhenUnset(t *testing.T) {
	body, err := buildRFC5322(mailer.Message{
		From:    "no-reply@example.test",
		To:      []string{"player@example.test"},
		Subject: "Verify",
		Body:    "hello",
	})

	require.NoError(t, err)
	assert.NotContains(t, string(body), "List-Unsubscribe")
}

func TestBuildRFC5322AcceptsDisplayNames(t *testing.T) {
	body, err := buildRFC5322(mailer.Message{
		From:    "GG Scale <no-reply@example.test>",
		To:      []string{"Player One <player@example.test>"},
		Subject: "Welcome",
		Body:    "hello",
	})

	require.NoError(t, err)
	msg := string(body)
	assert.True(t, strings.HasPrefix(msg, "From: GG Scale <no-reply@example.test>\r\n"))
	assert.Contains(t, msg, "To: Player One <player@example.test>\r\n")
	assert.Contains(t, msg, "Subject: Welcome\r\n")
}

// fakeServer is a plain-text SMTP server on a local port. It answers 250 to
// each command, except reject, which gets the reply line. It returns the
// address to dial.
func fakeServer(t *testing.T, reject, reply string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		tp := textproto.NewConn(conn)
		_ = tp.PrintfLine("220 fake ESMTP")
		inData := false
		for {
			line, err := tp.ReadLine()
			if err != nil {
				return
			}
			step := line
			if inData {
				if line != "." {
					continue
				}
				inData, step = false, "."
			}
			switch {
			case strings.HasPrefix(step, reject):
				_ = tp.PrintfLine("%s", reply)
			case strings.HasPrefix(step, "DATA"):
				inData = true
				_ = tp.PrintfLine("354 go on")
			case strings.HasPrefix(step, "QUIT"):
				_ = tp.PrintfLine("221 bye")
				return
			default:
				_ = tp.PrintfLine("250 ok")
			}
		}
	}()
	return ln.Addr().String()
}

func TestSend_errors_should_hold_the_step_and_code_but_no_address(t *testing.T) {
	const rcpt = "secret.person@example.org"
	cases := map[string]struct {
		reject, reply, want string
	}{
		"RCPT reply that repeats the address":   {"RCPT", "550 5.1.1 <" + rcpt + ">: Recipient address rejected", "mailer: RCPT: server replied 550"},
		"MAIL reply that repeats the addresses": {"MAIL", "553 5.7.1 <no-reply@example.test>: sender rejected for " + rcpt, "mailer: MAIL: server replied 553"},
		"end of DATA reply":                     {".", "554 5.7.1 message to " + rcpt + " refused", "mailer: close: server replied 554"},
		"malformed RCPT reply":                  {"RCPT", "5" + rcpt, "mailer: RCPT: malformed server reply"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := mailer.New("smtp", fakeServer(t, c.reject, c.reply), "", "", "no-reply@example.test", TLSModeOff)
			require.NoError(t, err)

			err = m.Send(context.Background(), mailer.Message{To: []string{"Secret Person <" + rcpt + ">"}, Subject: "Invite", Body: "hello"})

			require.Error(t, err)
			assert.Equal(t, c.want, err.Error())
		})
	}
}

func TestSend_should_not_quote_an_invalid_recipient(t *testing.T) {
	m, err := mailer.New("smtp", "127.0.0.1:1", "", "", "no-reply@example.test", TLSModeOff)
	require.NoError(t, err)

	err = m.Send(context.Background(), mailer.Message{To: []string{"secret.person@example.org, other.person@example.org"}, Subject: "Invite", Body: "hello"})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "person")
}

func TestBuildRFC5322_should_not_quote_a_refused_recipient(t *testing.T) {
	_, err := buildRFC5322(mailer.Message{From: "no-reply@example.test", To: []string{"secret.person@example.org\r\nBcc: x"}, Subject: "Invite", Body: "hello"})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret.person")
}
