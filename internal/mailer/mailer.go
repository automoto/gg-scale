// Package mailer sends transactional email. The dev compose stack runs
// MailHog at :1025 (SMTP) / :8025 (web UI) which captures every send
// without forwarding; production deploys swap in real SMTP.
package mailer

import (
	"context"
	"fmt"
	"net/smtp"
	"strings"
)

// Mailer abstracts the send so tests can substitute a recorder.
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

// Message is the minimum set of fields we need for Phase 1 transactional
// mail. Add CC/BCC/HTML when product needs them.
type Message struct {
	From    string
	To      []string
	Subject string
	Body    string
}

// SMTPMailer talks to a plain SMTP server. MailHog accepts unauthenticated
// connections; production uses STARTTLS via the standard library
// machinery in net/smtp.
type SMTPMailer struct {
	addr string
	auth smtp.Auth
}

// NewSMTPMailer constructs a mailer pointed at addr (e.g.
// "mailhog:1025" in dev). auth may be nil for unauthenticated relays.
func NewSMTPMailer(addr string, auth smtp.Auth) *SMTPMailer {
	return &SMTPMailer{addr: addr, auth: auth}
}

// Send encodes msg as RFC 5322 and hands it to the SMTP server. Context
// is currently advisory — net/smtp doesn't accept it; we may swap to
// go-mail later.
func (m *SMTPMailer) Send(_ context.Context, msg Message) error {
	body := buildRFC5322(msg)
	if err := smtp.SendMail(m.addr, m.auth, msg.From, msg.To, body); err != nil {
		return fmt.Errorf("mailer: send: %w", err)
	}
	return nil
}

func buildRFC5322(m Message) []byte {
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

// Recorder is a Mailer that captures every Send. Useful as a test double
// and in dev when the operator just wants to see what would have been
// sent without a real SMTP server.
type Recorder struct {
	Sent []Message
}

// Send records msg.
func (r *Recorder) Send(_ context.Context, msg Message) error {
	r.Sent = append(r.Sent, msg)
	return nil
}
