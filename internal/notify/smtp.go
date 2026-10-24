package notify

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
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
	// From may carry a name, "Acme <no-reply@example.com>", as a From
	// header does. The relay is handed only the address: given the whole of
	// it, a relay refuses every message, and every sign-in with it.
	var from *mail.Address
	if cfg.From == "" {
		errs = append(errs, errors.New("notify: SMTP from address is required"))
	} else if a, err := mail.ParseAddress(cfg.From); err != nil {
		errs = append(errs, fmt.Errorf("notify: SMTP from address %q is not an address, or a name and an address in <>: %w", cfg.From, err))
	} else {
		from = a
	}
	// A password with no username is a configuration slip that would otherwise
	// silently send unauthenticated.
	if cfg.Password != "" && cfg.Username == "" {
		errs = append(errs, errors.New("notify: SMTP password set without a username"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(cfg.Addr)
	return &smtpMailer{cfg: cfg, from: from, now: time.Now, implicitTLS: port == "465"}, nil
}

type smtpMailer struct {
	cfg  SMTPConfig
	from *mail.Address
	now  func() time.Time

	// implicitTLS is port 465's way: TLS from the first byte, rather than
	// STARTTLS after the greeting. Some relays offer only this one
	// (Cloudflare's), and to them a plain connection is never answered.
	implicitTLS bool
	// tlsConfig is nil but in tests, which trust their own relay.
	tlsConfig *tls.Config
}

// smtpTimeout bounds a send over implicit TLS, which net/smtp's SendMail
// would leave unbounded: a sign-in waits on it.
const smtpTimeout = 30 * time.Second

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

	if !m.implicitTLS {
		if err := smtp.SendMail(m.cfg.Addr, auth, m.from.Address,
			[]string{msg.To}, m.wire(msg)); err != nil {
			return fmt.Errorf("notify: smtp send: %w", err)
		}
		return nil
	}
	if err := m.sendImplicitTLS(host, auth, msg); err != nil {
		return fmt.Errorf("notify: smtp send: %w", err)
	}
	return nil
}

// sendImplicitTLS is SendMail over a connection that is TLS from the start.
func (m *smtpMailer) sendImplicitTLS(host string, auth smtp.Auth, msg Message) error {
	cfg := m.tlsConfig
	if cfg == nil {
		cfg = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: smtpTimeout}, "tcp", m.cfg.Addr, cfg)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(smtpTimeout))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(m.from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(msg.To); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(m.wire(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// wire builds the RFC 5322 message.
//
// Headers are written explicitly rather than with a template so that a newline
// in a subject cannot inject one: header injection through a display name is
// the classic mail bug. Date and Message-ID are there because mail without
// them is mail many providers file as spam, and a sign-in link in spam is a
// sign-in that does not work.
func (m *smtpMailer) wire(msg Message) []byte {
	var b strings.Builder
	b.WriteString("From: " + sanitizeHeader(m.from.String()) + "\r\n")
	b.WriteString("To: " + sanitizeHeader(msg.To) + "\r\n")
	b.WriteString("Subject: " + sanitizeHeader(msg.Subject) + "\r\n")
	b.WriteString("Date: " + m.now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("Message-ID: " + messageID(m.from.Address) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(msg.TextBody)
	return []byte(b.String())
}

func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// messageID is a fresh Message-ID on the sender's own domain, as RFC 5322
// asks: random, so no two messages share one.
func messageID(from string) string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	domain := "localhost"
	if at := strings.LastIndexByte(from, '@'); at >= 0 && at < len(from)-1 {
		domain = from[at+1:]
	}
	return "<" + hex.EncodeToString(buf[:]) + "@" + sanitizeHeader(domain) + ">"
}
