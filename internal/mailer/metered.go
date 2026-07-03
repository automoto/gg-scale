package mailer

import "context"

// SendRecorder records the outcome of a mail send. *observability.Metrics
// satisfies it (via MailSend); declaring it here keeps mailer free of an
// observability import.
type SendRecorder interface {
	MailSend(result string)
}

// Metered wraps inner so every Send increments a result-labelled counter.
// A silent mail failure is a launch risk (users never get verify/invite mail
// with no operator signal), so this makes the failure rate observable. Returns
// inner unchanged when rec is nil.
func Metered(inner Mailer, rec SendRecorder) Mailer {
	if rec == nil {
		return inner
	}
	return &meteredMailer{inner: inner, rec: rec}
}

type meteredMailer struct {
	inner Mailer
	rec   SendRecorder
}

// Send result labels. Kept in sync with observability.MailOK / MailError.
const (
	sendResultOK    = "ok"
	sendResultError = "error"
)

func (m *meteredMailer) Send(ctx context.Context, msg Message) error {
	err := m.inner.Send(ctx, msg)
	if err != nil {
		m.rec.MailSend(sendResultError)
		return err
	}
	m.rec.MailSend(sendResultOK)
	return nil
}
