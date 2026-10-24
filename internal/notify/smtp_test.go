package notify

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewSMTPRejectsIncompleteConfig(t *testing.T) {
	cases := map[string]SMTPConfig{
		"no address":          {From: "yacht@example.test"},
		"no from":             {Addr: "smtp.example.test:587"},
		"from not an address": {Addr: "smtp.example.test:587", From: "Acme no-reply"},
		"host only":           {Addr: "smtp.example.test", From: "yacht@example.test"},
		"password only": {
			Addr: "smtp.example.test:587", From: "yacht@example.test",
			Password: "secret",
		},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSMTP(cfg); err == nil {
				t.Fatalf("NewSMTP accepted %+v", cfg)
			}
		})
	}
}

func TestNewSMTPAcceptsCompleteConfig(t *testing.T) {
	for name, cfg := range map[string]SMTPConfig{
		"anonymous": {Addr: "smtp.example.test:587", From: "yacht@example.test"},
		"named":     {Addr: "smtp.example.test:587", From: "Acme <no-reply@example.test>"},
		"authenticated": {
			Addr: "smtp.example.test:587", From: "yacht@example.test",
			Username: "user", Password: "secret",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSMTP(cfg); err != nil {
				t.Fatalf("NewSMTP: %v", err)
			}
		})
	}
}

// A From with a name is handed to the relay as its address alone, and kept
// whole in the header. Given "Acme <no-reply@…>" as the envelope sender,
// a relay refuses the message, and with it every sign-in link.
func TestSMTPSendsANamedFromAsItsAddress(t *testing.T) {
	relay := newRelay(t)
	m, err := NewSMTP(SMTPConfig{Addr: relay.addr, From: "Acme <no-reply@example.test>"})
	if err != nil {
		t.Fatalf("NewSMTP: %v", err)
	}
	if err := m.Send(context.Background(), Message{
		To: "amina@example.test", Subject: "Sign in", TextBody: "Your link.",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := <-relay.got
	if got.from != "<no-reply@example.test>" {
		t.Errorf("MAIL FROM:%s, want the bare address", got.from)
	}
	for _, want := range []string{
		"From: \"Acme\" <no-reply@example.test>\r\n",
		"\r\nDate: ", "\r\nMessage-ID: <", "@example.test>\r\n",
	} {
		if !strings.Contains(got.data, want) {
			t.Errorf("message lacks %q:\n%s", want, got.data)
		}
	}
}

// Port 465 is TLS from the first byte, as Cloudflare's relay is: a mailer
// that greets it in plain text waits for an answer that never comes.
func TestSMTPOnPort465SpeaksTLSFromTheStart(t *testing.T) {
	m, err := NewSMTP(SMTPConfig{Addr: "smtp.example.test:465", From: "no-reply@example.test"})
	if err != nil {
		t.Fatalf("NewSMTP: %v", err)
	}
	if !m.(*smtpMailer).implicitTLS {
		t.Error("port 465 does not use implicit TLS")
	}
	m, _ = NewSMTP(SMTPConfig{Addr: "smtp.example.test:587", From: "no-reply@example.test"})
	if m.(*smtpMailer).implicitTLS {
		t.Error("port 587 uses implicit TLS; it is STARTTLS's")
	}

	ts := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	relay := newRelay(t, func(l net.Listener) net.Listener { return tls.NewListener(l, ts.TLS) })
	m, err = NewSMTP(SMTPConfig{
		Addr: relay.addr, Username: "api_token", Password: "s3cret", From: "Acme <no-reply@example.test>",
	})
	if err != nil {
		t.Fatalf("NewSMTP: %v", err)
	}
	sm := m.(*smtpMailer)
	sm.implicitTLS = true
	sm.tlsConfig = &tls.Config{
		RootCAs:    ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
		ServerName: "127.0.0.1",
	}
	if err := m.Send(context.Background(), Message{
		To: "amina@example.test", Subject: "Sign in", TextBody: "Your link.",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := <-relay.got
	if want := base64.StdEncoding.EncodeToString([]byte("\x00api_token\x00s3cret")); got.auth != "PLAIN "+want {
		t.Errorf("AUTH %q, want PLAIN with the username and password", got.auth)
	}
	if got.from != "<no-reply@example.test>" || !strings.Contains(got.data, "Your link.") {
		t.Errorf("relayed %+v", got)
	}
}

type relayed struct{ auth, from, data string }

type relay struct {
	addr string
	got  chan relayed
}

// newRelay is just enough of an SMTP server for one message, without
// STARTTLS, recording the login, the envelope sender and the message. wrap,
// when given, wraps its listener (in TLS, say).
func newRelay(t *testing.T, wrap ...func(net.Listener) net.Listener) *relay {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	addr := l.Addr().String()
	for _, w := range wrap {
		l = w(l)
	}
	r := &relay{addr: addr, got: make(chan relayed, 1)}
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		in := bufio.NewReader(c)
		say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
		say("220 relay.test")
		var msg relayed
		for {
			line, err := in.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250 relay.test")
			case strings.HasPrefix(cmd, "AUTH "):
				msg.auth = strings.TrimSpace(line)[len("AUTH "):]
				say("235 ok")
			case strings.HasPrefix(cmd, "MAIL FROM:"):
				msg.from = strings.TrimSpace(line)[len("MAIL FROM:"):]
				say("250 ok")
			case strings.HasPrefix(cmd, "RCPT TO:"):
				say("250 ok")
			case cmd == "DATA":
				say("354 go on")
				var b strings.Builder
				for {
					l, err := in.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					b.WriteString(l)
				}
				msg.data = b.String()
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				r.got <- msg
				return
			default:
				say("250 ok")
			}
		}
	}()
	return r
}
