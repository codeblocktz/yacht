package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
)

// SMTPConfig describes an SMTP relay.
type SMTPConfig struct {
	Addr     string // host:port
	Username string
	Password string
	From     string
}

// NewSMTP returns a Mailer that sends through an SMTP relay.
//
// Configuration is validated here rather than at first send, because the first
// send is a sign-in attempt — and a relay misconfiguration discovered then
// looks to the operator like sign-in being broken.
func NewSMTP(cfg SMTPConfig) (Mailer, error) {
	var errs []error
	if cfg.Addr == "" {
		errs = append(errs, errors.New("notify: SMTP address is required"))
	} else if _, _, err := net.SplitHostPort(cfg.Addr); err != nil {
		errs = append(errs, fmt.Errorf("notify: SMTP address must be host:port: %w", err))
	}
	if cfg.From == "" {
		errs = append(errs, errors.New("notify: SMTP from address is required"))
	}
	// A password with no username is a configuration slip that would otherwise
	// silently send unauthenticated.
	if cfg.Password != "" && cfg.Username == "" {
		errs = append(errs, errors.New("notify: SMTP password set without a username"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &smtpMailer{cfg: cfg}, nil
}

type smtpMailer struct{ cfg SMTPConfig }

func (m *smtpMailer) Send(_ context.Context, msg Message) error {
	if err := msg.Validate(); err != nil {
		return err
	}

	host, _, err := net.SplitHostPort(m.cfg.Addr)
	if err != nil {
		return fmt.Errorf("notify: smtp address: %w", err)
	}

	var auth smtp.Auth
	if m.cfg.Username != "" {
		auth = smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, host)
	}

	if err := smtp.SendMail(m.cfg.Addr, auth, m.cfg.From,
		[]string{msg.To}, m.wire(msg)); err != nil {
		return fmt.Errorf("notify: smtp send: %w", err)
	}
	return nil
}

// wire builds the RFC 5322 message.
//
// Headers are written explicitly rather than with a template so that a newline
// in a subject cannot inject one: header injection through a display name is
// the classic mail bug.
func (m *smtpMailer) wire(msg Message) []byte {
	var b strings.Builder
	b.WriteString("From: " + sanitizeHeader(m.cfg.From) + "\r\n")
	b.WriteString("To: " + sanitizeHeader(msg.To) + "\r\n")
	b.WriteString("Subject: " + sanitizeHeader(msg.Subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(msg.TextBody)
	return []byte(b.String())
}

func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}
