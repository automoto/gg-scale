package smtp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/mailer"
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
