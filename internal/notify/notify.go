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
	"strconv"
	"strings"
	"time"
)

// Message is what the engine wants delivered.
//
// Written as text. The HTML part a transport sends alongside is derived from
// it (see HTML), so the two can never say different things: a control plane's
// mail is short and transactional, and a second hand-written body is a second
// place for the wording to drift.
type Message struct {
	To       string
	Subject  string
	TextBody string

	// Action is the one thing the mail asks its reader to do — sign in,
	// accept an invitation. TextBody carries Action.URL as a paragraph of its
	// own; Text puts the label above it and HTML draws it as a button with the
	// address underneath, for a client that shows neither.
	Action *Action

	// Reason says why this address was sent the mail, in the footer: "You're
	// receiving this because …". A mail that cannot say why it came reads as
	// one nobody asked for.
	Reason string

	// Introduces marks a mail that introduces the product to somebody new —
	// a sign-up link, an invitation, a welcome — and so carries the install's
	// Footer.Intro, when it has one. A sign-in link or a bill does not.
	Introduces bool

	// HTMLContent, when set, is the HTML part's content in place of the
	// paragraphs drawn from TextBody: a receipt's table, say, or a sender's
	// logo. The frame and the footer stay the engine's. Whoever sets it owns
	// keeping it saying what TextBody says, and escaping what goes in it;
	// TextBody is still required and is still the plain-text part.
	HTMLContent string

	// Footer and ReplyTo are the install's, filled in by WithBranding rather
	// than by whoever composes the message.
	Footer  Footer
	ReplyTo string
}

// Action is a mail's button: what it says, and where it goes.
type Action struct {
	Label string
	URL   string
}

// Footer is who sends an install's mail, as its footer says.
type Footer struct {
	// Brand is the product's name, "datahuru".
	Brand string
	// Company is who runs it and where — "hosting by Codeblock Limited, Dar
	// es Salaam, Tanzania". Empty leaves the line at the brand alone.
	Company string
	// Website is the product's public site, linked at the very end.
	Website string
	// Intro is a paragraph for the end of the mails that introduce the
	// product (Message.Introduces): what a newcomer should know about it
	// that a regular already does. Empty adds nothing.
	Intro string
}

// Text is the message's plain-text part: TextBody, with the action labelled
// and the footer under a signature separator.
func (m Message) Text() string {
	if m.Action == nil && len(m.footerLines()) == 0 {
		return m.TextBody
	}
	body := strings.TrimRight(strings.ReplaceAll(m.TextBody, "\r\n", "\n"), "\n")
	if a := m.Action; a != nil && a.URL != "" {
		paras := strings.Split(body, "\n\n")
		for i, p := range paras {
			if strings.TrimSpace(p) == a.URL {
				paras[i] = a.Label + ":\n" + a.URL
			}
		}
		body = strings.Join(paras, "\n\n")
	}
	if lines := m.footerLines(); len(lines) > 0 {
		// "-- " is the signature separator mail clients know to fold away.
		body += "\n\n-- \n" + strings.Join(lines, "\n")
	}
	return body + "\n"
}

func (m Message) footerLines() []string {
	var lines []string
	if f := m.Footer; f.Brand != "" {
		line := f.Brand
		if f.Company != "" {
			line += " — " + f.Company
		}
		lines = append(lines, line)
	}
	if m.Reason != "" {
		lines = append(lines, m.Reason)
	}
	if m.Footer.Website != "" {
		lines = append(lines, m.Footer.Website)
	}
	return lines
}

// WithBranding returns a Mailer that signs every message with the install's
// footer and Reply-To before handing it to next. One place, so a mail
// composed anywhere — the engine's or a wrapper's — is signed alike.
func WithBranding(next Mailer, f Footer, replyTo string) Mailer {
	return &brandingMailer{next: next, footer: f, replyTo: replyTo}
}

type brandingMailer struct {
	next    Mailer
	footer  Footer
	replyTo string
}

func (m *brandingMailer) Send(ctx context.Context, msg Message) error {
	if msg.Footer == (Footer{}) {
		msg.Footer = m.footer
	}
	if msg.ReplyTo == "" {
		msg.ReplyTo = m.replyTo
	}
	// Into the body rather than the footer: it is said to the reader, and a
	// footer is folded away by the mail clients that know the separator.
	if intro := strings.TrimSpace(msg.Footer.Intro); msg.Introduces && intro != "" {
		msg.TextBody = strings.TrimRight(msg.TextBody, "\r\n") + "\n\n" + intro + "\n"
	}
	return m.next.Send(ctx, msg)
}

// Duration is d as a person says it: "15 minutes", "1 hour 30 minutes",
// "30 days". Go's own "15m0s" reads as a typo in a mail, and a mail that reads
// as machine output is a mail filters and people both distrust.
func Duration(d time.Duration) string {
	if d < time.Minute {
		return plural(int(d.Round(time.Second)/time.Second), "second")
	}
	d = d.Round(time.Minute)
	days, d := d/(24*time.Hour), d%(24*time.Hour)
	hours, d := d/time.Hour, d%time.Hour
	mins := d / time.Minute
	var parts []string
	if days > 0 {
		parts = append(parts, plural(int(days), "day"))
	}
	if hours > 0 {
		parts = append(parts, plural(int(hours), "hour"))
	}
	if mins > 0 && days == 0 {
		parts = append(parts, plural(int(mins), "minute"))
	}
	return strings.Join(parts, " ")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
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
		slog.String("body", msg.Text()),
	)
	return nil
}

// MailerFunc is a function as a Mailer.
type MailerFunc func(ctx context.Context, msg Message) error

// Send calls f.
func (f MailerFunc) Send(ctx context.Context, msg Message) error { return f(ctx, msg) }
