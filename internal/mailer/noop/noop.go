// Package noop registers the "noop" mailer provider, which silently discards
// all mail. Useful for self-hosters who don't need email, and for CI
// environments. Import for side-effects:
//
//	import _ "github.com/ggscale/ggscale/internal/mailer/noop"
package noop

import (
	"context"

	"github.com/ggscale/ggscale/internal/mailer"
)

func init() {
	mailer.Register("noop", func(_, _, _, _ string) (mailer.Mailer, error) {
		return &noopMailer{}, nil
	})
}

type noopMailer struct{}

func (*noopMailer) Send(_ context.Context, _ mailer.Message) error { return nil }
