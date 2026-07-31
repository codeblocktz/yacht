package notify

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestLogMailerWritesTheMessage(t *testing.T) {
	var buf bytes.Buffer
	m := NewLog(slog.New(slog.NewTextHandler(&buf, nil)))

	err := m.Send(context.Background(), Message{
		To:       "someone@example.test",
		Subject:  "Sign in to Yacht",
		TextBody: "https://yacht.example.test/auth/abc123",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	out := buf.String()
	// The body has to be logged in full. This is the break-glass path when no
	// mail transport is configured — a truncated link is no way back in.
	for _, want := range []string{
		"someone@example.test", "Sign in to Yacht",
		"https://yacht.example.test/auth/abc123",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q\ngot: %s", want, out)
		}
	}
}

func TestMessageValidation(t *testing.T) {
	m := NewLog(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	for name, msg := range map[string]Message{
		"no recipient": {Subject: "s", TextBody: "b"},
		"no subject":   {To: "a@b.test", TextBody: "b"},
		"no body":      {To: "a@b.test", Subject: "s"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := m.Send(context.Background(), msg); err == nil {
				t.Fatal("Send accepted an incomplete message")
			}
		})
	}
}
