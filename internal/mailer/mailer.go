// Package mailer sends transactional email. The dev compose stack runs
// MailHog at :1025 (SMTP) / :8025 (web UI) which captures every send
// without forwarding.
//
// Providers register themselves with Register in their package init, following
// the same database/sql driver pattern. The built-in providers are in the smtp
// and noop sub-packages; import them for side-effects in main.go:
//
//	import _ "github.com/ggscale/ggscale/internal/mailer/noop"
//	import _ "github.com/ggscale/ggscale/internal/mailer/smtp"
//
// External providers (e.g. SendGrid, Mailgun) follow the same pattern without
// ggscale ever importing them — no vendor lock-in enters the core binary.
package mailer

import (
	"context"
	"fmt"
	"sync"
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

// ProviderFunc constructs a Mailer from connection parameters.
// addr is the server address (host:port for SMTP, endpoint URL for managed
// providers). user and password are credentials — may be empty for
// unauthenticated relays like MailHog. from is the default sender address.
//
// External providers implement this signature and call Register in init().
type ProviderFunc func(addr, user, password, from string) (Mailer, error)

var (
	mu        sync.RWMutex
	providers = map[string]ProviderFunc{}
)

// Register makes a provider available under name. Typically called from
// provider init() functions. Panics if the same name is registered twice to
// surface wiring mistakes early.
func Register(name string, fn ProviderFunc) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := providers[name]; ok {
		panic(fmt.Sprintf("mailer: provider %q already registered", name))
	}
	providers[name] = fn
}

// New constructs the named provider. Returns an error if the provider was
// never registered — typically means the import side-effect is missing in
// main.go.
func New(provider, addr, user, password, from string) (Mailer, error) {
	mu.RLock()
	fn, ok := providers[provider]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("mailer: unknown provider %q (did you import the provider package?)", provider)
	}
	return fn(addr, user, password, from)
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
