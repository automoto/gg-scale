package projectadmin

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHeaderSafeName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain name kept", "Doomerang", "Doomerang"},
		{"unicode kept", "Süper Güme 🎮", "Süper Güme 🎮"},
		{"newline falls back", "Evil\nGame", "fallback"},
		{"carriage return falls back", "Evil\rBcc: x", "fallback"},
		{"empty falls back", "", "fallback"},
		{"oversized falls back", strings.Repeat("a", 200), "fallback"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, HeaderSafeName(tc.in, "fallback"))
		})
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestPlayerInviterSend_should_log_the_domain_and_not_the_address_without_a_mailer(t *testing.T) {
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	(&PlayerInviter{}).send(context.Background(), PlayerInvite{ID: 42, Email: "secret.person@example.org"}, inviteMail{}, 1)

	out := buf.String()
	assert.Contains(t, out, "invite_id=42")
	assert.Contains(t, out, "domain=example.org")
	assert.NotContains(t, out, "secret.person")
}
