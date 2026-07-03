package mailer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubMailer struct{ err error }

func (s stubMailer) Send(context.Context, Message) error { return s.err }

type recorder struct{ results []string }

func (r *recorder) MailSend(result string) { r.results = append(r.results, result) }

func TestMetered_records_ok_on_success(t *testing.T) {
	rec := &recorder{}
	m := Metered(stubMailer{}, rec)

	require.NoError(t, m.Send(context.Background(), Message{}))
	assert.Equal(t, []string{sendResultOK}, rec.results)
}

func TestMetered_records_error_and_propagates(t *testing.T) {
	rec := &recorder{}
	sendErr := errors.New("smtp down")
	m := Metered(stubMailer{err: sendErr}, rec)

	err := m.Send(context.Background(), Message{})
	assert.ErrorIs(t, err, sendErr, "the underlying error must still propagate")
	assert.Equal(t, []string{sendResultError}, rec.results)
}

func TestMetered_nil_recorder_returns_inner(t *testing.T) {
	inner := stubMailer{}
	assert.Equal(t, Mailer(inner), Metered(inner, nil), "nil recorder is a pass-through")
}
