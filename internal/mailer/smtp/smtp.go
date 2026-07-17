// Package smtp registers the "smtp" mailer provider. Import for side-effects:
//
//	import _ "github.com/ggscale/ggscale/internal/mailer/smtp"
package smtp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strings"
	"time"

	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/webutil"
)

// TLS modes accepted by SMTP_TLS. starttls is the default and fails the
// send if the server doesn't advertise STARTTLS — no silent downgrade.
const (
	TLSModeOff      = "off"
	TLSModeSTARTTLS = "starttls"
	TLSModeImplicit = "implicit"
	sendTimeout     = 15 * time.Second
)

func init() {
	mailer.Register("smtp", func(addr, user, password, from, tlsMode string) (mailer.Mailer, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("smtp: invalid addr %q: %w", addr, err)
		}
		var auth smtp.Auth
		if user != "" {
			auth = smtp.PlainAuth("", user, password, host)
		}
		if tlsMode == "" {
			tlsMode = TLSModeSTARTTLS
		}
		switch tlsMode {
		case TLSModeOff, TLSModeSTARTTLS, TLSModeImplicit:
		default:
			return nil, fmt.Errorf("smtp: unknown tls mode %q (want off|starttls|implicit)", tlsMode)
		}
		localName, err := os.Hostname()
		if err != nil || localName == "" {
			localName = "localhost"
		}
		return &smtpMailer{
			addr:      addr,
			host:      host,
			localName: localName,
			auth:      auth,
			from:      from,
			tlsMode:   tlsMode,
		}, nil
	})
}

type smtpMailer struct {
	addr      string
	host      string
	localName string
	auth      smtp.Auth
	from      string
	tlsMode   string
}

// Send encodes msg as RFC 5322 and hands it to the SMTP server. Header
// fields are passed through webutil.SanitizeHeader so CRLF/control-char
// injection is rejected at the boundary.
func (m *smtpMailer) Send(ctx context.Context, msg mailer.Message) error {
	if msg.From == "" {
		msg.From = m.from
	}
	fromAddr, toAddr, err := parseEnvelope(msg)
	if err != nil {
		return err
	}
	body, err := buildRFC5322(msg)
	if err != nil {
		return fmt.Errorf("mailer: build message: %w", err)
	}
	switch m.tlsMode {
	case TLSModeImplicit:
		return m.sendImplicit(ctx, fromAddr, toAddr, body)
	default:
		return m.sendSTARTTLSOrPlain(ctx, fromAddr, toAddr, body, m.tlsMode == TLSModeSTARTTLS)
	}
}

// sendSTARTTLSOrPlain dials cleartext then upgrades via STARTTLS when
// requireTLS is true. With requireTLS=false the connection stays in
// cleartext (only acceptable for off-network Mailpit and other dev relays).
func (m *smtpMailer) sendSTARTTLSOrPlain(ctx context.Context, from string, to []string, body []byte, requireTLS bool) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", m.addr)
	if err != nil {
		return fmt.Errorf("mailer: dial: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(sendTimeout)); err != nil {
		return fmt.Errorf("mailer: deadline: %w", err)
	}
	c, err := smtp.NewClient(conn, m.host)
	if err != nil {
		return fmt.Errorf("mailer: smtp.NewClient: %w", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Hello(m.localName); err != nil {
		return fmt.Errorf("mailer: HELO: %w", err)
	}
	if requireTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("mailer: server does not advertise STARTTLS (set SMTP_TLS=off to permit cleartext)")
		}
		if err := c.StartTLS(&tls.Config{ServerName: m.host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("mailer: STARTTLS: %w", err)
		}
	}
	if m.auth != nil {
		if err := c.Auth(m.auth); err != nil {
			return fmt.Errorf("mailer: auth: %w", err)
		}
	}
	return writeMessage(c, from, to, body)
}

// sendImplicit dials TLS from connect (typically port 465).
func (m *smtpMailer) sendImplicit(ctx context.Context, from string, to []string, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	conn, err := (&tls.Dialer{Config: &tls.Config{ServerName: m.host, MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", m.addr)
	if err != nil {
		return fmt.Errorf("mailer: tls dial: %w", err)
	}
	if err := conn.SetDeadline(time.Now().Add(sendTimeout)); err != nil {
		_ = conn.Close()
		return fmt.Errorf("mailer: deadline: %w", err)
	}
	c, err := smtp.NewClient(conn, m.host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mailer: smtp.NewClient: %w", err)
	}
	defer func() { _ = c.Close() }()
	if m.auth != nil {
		if err := c.Auth(m.auth); err != nil {
			return fmt.Errorf("mailer: auth: %w", err)
		}
	}
	return writeMessage(c, from, to, body)
}

func parseEnvelope(m mailer.Message) (string, []string, error) {
	from, err := mail.ParseAddress(m.From)
	if err != nil {
		return "", nil, fmt.Errorf("from address: %w", err)
	}
	to := make([]string, 0, len(m.To))
	for _, rcpt := range m.To {
		parsed, err := mail.ParseAddress(rcpt)
		if err != nil {
			return "", nil, fmt.Errorf("to address %q: %w", rcpt, err)
		}
		to = append(to, parsed.Address)
	}
	return from.Address, to, nil
}

func writeMessage(c *smtp.Client, from string, to []string, body []byte) error {
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("mailer: MAIL: %w", err)
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("mailer: RCPT %q: %w", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mailer: DATA: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("mailer: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mailer: close: %w", err)
	}
	return c.Quit()
}

// buildRFC5322 sanitises every header field through webutil.SanitizeHeader.
// CR/LF/null/control characters are rejected outright — a malformed value
// here was the previous header-injection vector (an attacker-controlled
// display name could splice in Bcc:, Cc:, or split the message).
func buildRFC5322(m mailer.Message) ([]byte, error) {
	fromHeader, err := webutil.SanitizeHeader(m.From)
	if err != nil {
		return nil, fmt.Errorf("from header: %w", err)
	}
	subjectHeader, err := webutil.SanitizeHeader(m.Subject)
	if err != nil {
		return nil, fmt.Errorf("subject header: %w", err)
	}
	toClean := make([]string, 0, len(m.To))
	for _, rcpt := range m.To {
		cleaned, err := webutil.SanitizeHeader(rcpt)
		if err != nil {
			return nil, fmt.Errorf("to header %q: %w", rcpt, err)
		}
		toClean = append(toClean, cleaned)
	}
	var b strings.Builder
	b.WriteString("From: ")
	b.WriteString(fromHeader)
	b.WriteString("\r\n")
	b.WriteString("To: ")
	b.WriteString(strings.Join(toClean, ", "))
	b.WriteString("\r\n")
	b.WriteString("Subject: ")
	b.WriteString(subjectHeader)
	b.WriteString("\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(m.Body)
	return []byte(b.String()), nil
}
