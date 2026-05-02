// Package smtp registers the "smtp" mailer provider. Import for side-effects:
//
//	import _ "github.com/ggscale/ggscale/internal/mailer/smtp"
package smtp

import (
	"context"
	"fmt"
	"net"
	"net/smtp"
	"strings"

	"github.com/ggscale/ggscale/internal/mailer"
)

func init() {
	mailer.Register("smtp", func(addr, user, password, from string) (mailer.Mailer, error) {
		var auth smtp.Auth
		if user != "" {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("smtp: invalid addr %q: %w", addr, err)
			}
			auth = smtp.PlainAuth("", user, password, host)
		}
		return &smtpMailer{addr: addr, auth: auth, from: from}, nil
	})
}

type smtpMailer struct {
	addr string
	auth smtp.Auth
	from string
}

// Send encodes msg as RFC 5322 and hands it to the SMTP server. Context is
// currently advisory — net/smtp doesn't accept it; we may swap to go-mail later.
func (m *smtpMailer) Send(_ context.Context, msg mailer.Message) error {
	if msg.From == "" {
		msg.From = m.from
	}
	body := buildRFC5322(msg)
	if err := smtp.SendMail(m.addr, m.auth, msg.From, msg.To, body); err != nil {
		return fmt.Errorf("mailer: send: %w", err)
	}
	return nil
}

func buildRFC5322(m mailer.Message) []byte {
	var b strings.Builder
	b.WriteString("From: ")
	b.WriteString(m.From)
	b.WriteString("\r\n")
	b.WriteString("To: ")
	b.WriteString(strings.Join(m.To, ", "))
	b.WriteString("\r\n")
	b.WriteString("Subject: ")
	b.WriteString(m.Subject)
	b.WriteString("\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(m.Body)
	return []byte(b.String())
}
