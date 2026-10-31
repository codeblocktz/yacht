package notify

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDurationIsHowAPersonSaysIt(t *testing.T) {
	for d, want := range map[time.Duration]string{
		15 * time.Minute:                "15 minutes",
		time.Minute:                     "1 minute",
		time.Hour:                       "1 hour",
		90 * time.Minute:                "1 hour 30 minutes",
		7 * 24 * time.Hour:              "7 days",
		24*time.Hour + 2*time.Hour:      "1 day 2 hours",
		30 * time.Second:                "30 seconds",
		720 * time.Hour:                 "30 days",
		15*time.Minute + 20*time.Second: "15 minutes",
	} {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%v) = %q, want %q", d, got, want)
		}
	}
}

// The action is labelled in the text and a button in the HTML, and both
// still show the address itself.
func TestActionIsAButtonWithItsAddressBeneath(t *testing.T) {
	m := Message{
		Subject:  "Sign in",
		TextBody: "Hello.\n\nhttps://app.example.test/auth/x\n\nBye.\n",
		Action:   &Action{Label: "Sign in to Acme", URL: "https://app.example.test/auth/x"},
	}
	if text := m.Text(); !strings.Contains(text, "Sign in to Acme:\nhttps://app.example.test/auth/x\n") {
		t.Errorf("text:\n%s", text)
	}
	h := m.HTML()
	for _, want := range []string{
		"<title>Sign in</title>",
		`style="display:inline-block;padding:12px 20px;`,
		">Sign in to Acme</a>",
		`>https://app.example.test/auth/x</a></p>`,
	} {
		if !strings.Contains(h, want) {
			t.Errorf("html lacks %q:\n%s", want, h)
		}
	}
	if strings.Contains(h, "<img") {
		t.Error("html carries an image")
	}
}

// Branding signs a message once, at the install's word, and leaves one
// already signed alone.
func TestWithBrandingSignsEveryMessage(t *testing.T) {
	var got Message
	m := WithBranding(MailerFunc(func(_ context.Context, msg Message) error { got = msg; return nil }),
		Footer{Brand: "Acme", Company: "hosting by Acme Ltd, Nairobi", Website: "https://acme.test"},
		"help@acme.test")
	_ = m.Send(context.Background(), Message{To: "a@b.test", Subject: "s", TextBody: "Body.", Reason: "You asked."})
	if got.ReplyTo != "help@acme.test" {
		t.Errorf("ReplyTo = %q", got.ReplyTo)
	}
	want := "Body.\n\n-- \nAcme — hosting by Acme Ltd, Nairobi\nYou asked.\nhttps://acme.test\n"
	if got.Text() != want {
		t.Errorf("text = %q, want %q", got.Text(), want)
	}
}

func TestSMTPSendsTextAndHTMLWithAReplyTo(t *testing.T) {
	relay := newRelay(t)
	m, err := NewSMTP(SMTPConfig{Addr: relay.addr, From: "Acme <no-reply@example.test>"})
	if err != nil {
		t.Fatalf("NewSMTP: %v", err)
	}
	if err := m.Send(context.Background(), Message{
		To: "amina@example.test", Subject: "Sign in", TextBody: "Your link.", ReplyTo: "help@example.test",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := <-relay.got
	for _, want := range []string{
		"\r\nReply-To: help@example.test\r\n",
		"Content-Type: multipart/alternative; boundary=",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Type: text/html; charset=utf-8",
	} {
		if !strings.Contains(got.data, want) {
			t.Errorf("message lacks %q:\n%s", want, got.data)
		}
	}
}

// The install's introduction ends the mails that introduce the product, and
// only those: a sign-in link or a bill goes out as it was written.
func TestWithBrandingIntroducesTheProductToNewcomersOnly(t *testing.T) {
	var got Message
	m := WithBranding(MailerFunc(func(_ context.Context, msg Message) error { got = msg; return nil }),
		Footer{Brand: "Acme (preview)", Intro: "Acme is new: tell us what you need."}, "")

	_ = m.Send(context.Background(), Message{To: "a@b.test", Subject: "s", TextBody: "Welcome.\n", Introduces: true})
	want := "Welcome.\n\nAcme is new: tell us what you need.\n\n-- \nAcme (preview)\n"
	if got.Text() != want {
		t.Errorf("an introduction = %q, want %q", got.Text(), want)
	}
	if !strings.Contains(got.HTML(), "Acme is new: tell us what you need.") {
		t.Error("the HTML part leaves the introduction out")
	}

	_ = m.Send(context.Background(), Message{To: "a@b.test", Subject: "s", TextBody: "Your link."})
	if strings.Contains(got.Text(), "Acme is new") {
		t.Errorf("a transactional mail carries the introduction: %q", got.Text())
	}

	// No introduction set: an introducing mail is left as written.
	m = WithBranding(MailerFunc(func(_ context.Context, msg Message) error { got = msg; return nil }),
		Footer{Brand: "Acme"}, "")
	_ = m.Send(context.Background(), Message{To: "a@b.test", Subject: "s", TextBody: "Welcome.", Introduces: true})
	if got.TextBody != "Welcome." {
		t.Errorf("with no introduction the body became %q", got.TextBody)
	}
}
