// Package notify delivers messages to people.
//
// SEAM 4 of 4. The engine sends a small, fixed set of messages — a sign-in
// link, an invitation — and does not care how they arrive. An application
// wrapping the engine supplies Slack, Discord, or a queue by implementing one
// method, without touching anything that composes a message.
package notify

import (
	"context"
	"errors"
	"log/slog"
)

// Message is what the engine wants delivered.
//
// Text only. A control plane's mail is short and transactional, and an HTML
// body is a rendering surface with nothing to gain from it.
type Message struct {
	To       string
	Subject  string
	TextBody string
}

// Validate reports whether the message is deliverable.
func (m Message) Validate() error {
	switch {
	case m.To == "":
		return errors.New("notify: message has no recipient")
	case m.Subject == "":
		return errors.New("notify: message has no subject")
	case m.TextBody == "":
		return errors.New("notify: message has no body")
	}
	return nil
}

// Mailer delivers a message.
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

// NewLog returns a Mailer that writes messages to the log.
//
// This is the zero-configuration default, and deliberately not an error. It is
// the break-glass path: if accounts are switched on and mail delivery then
// breaks, the sign-in link in the log is the only way back into the dashboard.
// That is safe only because reading the log already implies host access, and it
// is the reason the whole body is logged rather than a summary.
func NewLog(log *slog.Logger) Mailer {
	if log == nil {
		log = slog.Default()
	}
	return &logMailer{log: log}
}

type logMailer struct{ log *slog.Logger }

func (m *logMailer) Send(_ context.Context, msg Message) error {
	if err := msg.Validate(); err != nil {
		return err
	}
	m.log.Info("no mail transport configured — message written to the log instead",
		slog.String("to", msg.To),
		slog.String("subject", msg.Subject),
		slog.String("body", msg.TextBody),
	)
	return nil
}
