package httpapi

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/secretseal"
)

func TestDecodeQueueCursor_should_refuse_every_changed_cursor_text(t *testing.T) {
	// Arrange
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	cipher, err := secretseal.NewCipherKeyring(make([]byte, 32))
	require.NoError(t, err)
	cursor, err := encodeQueueCursor(cipher, queueCursor{Mode: "match_only", Region: "eu", GameMode: "duel", Status: "queued"})
	require.NoError(t, err)
	head, tail := cursor[:len(cursor)-1], cursor[len(cursor)-1:]
	changed := []string{cursor + "\n", "\n" + cursor}
	for _, c := range alphabet {
		if string(c) != tail {
			changed = append(changed, head+string(c))
		}
	}

	// Act
	var accepted []string
	for _, s := range changed {
		if _, ok := decodeQueueCursor(cipher, s); ok {
			accepted = append(accepted, strings.TrimPrefix(s, head))
		}
	}

	// Assert
	assert.Empty(t, accepted)
}

func TestDecodeQueueCursor_should_accept_the_cursor_it_encoded(t *testing.T) {
	// Arrange
	cipher, err := secretseal.NewCipherKeyring(make([]byte, 32))
	require.NoError(t, err)
	want := queueCursor{Mode: "match_only", Region: "eu", GameMode: "duel", Status: "queued"}
	cursor, err := encodeQueueCursor(cipher, want)
	require.NoError(t, err)

	// Act
	got, ok := decodeQueueCursor(cipher, cursor)

	// Assert
	assert.Equal(t, []any{want, true}, []any{got, ok})
}
