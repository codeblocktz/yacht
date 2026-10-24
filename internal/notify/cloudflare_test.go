package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewCloudflareRejectsIncompleteConfig(t *testing.T) {
	for _, c := range []struct{ account, token, from string }{
		{"", "tok", "a@example.test"},
		{"acct", "", "a@example.test"},
		{"acct", "tok", ""},
		{"acct/../x", "tok", "a@example.test"},
	} {
		if _, err := NewCloudflare(c.account, c.token, c.from); err == nil {
			t.Errorf("NewCloudflare(%q, %q, %q) accepted it", c.account, c.token, c.from)
		}
	}
}

func TestCloudflarePostsTheMessage(t *testing.T) {
	var gotAuth, gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"success":true,"errors":[],"result":{"queued":["someone@example.test"],"permanent_bounces":[],"suppressed_recipients":[]}}`))
	}))
	defer srv.Close()

	m, err := NewCloudflare("acct123", "tok", "Acme <no-reply@example.test>")
	if err != nil {
		t.Fatalf("NewCloudflare: %v", err)
	}
	if want := "https://api.cloudflare.com/client/v4/accounts/acct123/email/sending/send"; m.(*cloudflareMailer).endpoint != want {
		t.Errorf("endpoint = %s, want %s", m.(*cloudflareMailer).endpoint, want)
	}
	m.(*cloudflareMailer).endpoint = srv.URL + "/accounts/acct123/email/sending/send"

	if err := m.Send(context.Background(), Message{
		To: "someone@example.test", Subject: "Sign in", TextBody: "link",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotPath != "/accounts/acct123/email/sending/send" {
		t.Errorf("path = %s", gotPath)
	}
	if body["from"] != "Acme <no-reply@example.test>" || body["subject"] != "Sign in" || body["text"] != "link" {
		t.Errorf("body = %v", body)
	}
	if to, _ := body["to"].([]any); len(to) != 1 || to[0] != "someone@example.test" {
		t.Errorf("to = %v", body["to"])
	}
}

// A failed send must say so, in Cloudflare's words.
func TestCloudflareReportsAnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10102,"message":"email.sending.error.authentication.forbidden"}],"result":null}`))
	}))
	defer srv.Close()

	m, _ := NewCloudflare("acct", "tok", "a@example.test")
	m.(*cloudflareMailer).endpoint = srv.URL
	err := m.Send(context.Background(), Message{To: "someone@example.test", Subject: "Sign in", TextBody: "link"})
	if err == nil || !strings.Contains(err.Error(), "authentication.forbidden (10102)") {
		t.Errorf("Send = %v, want the API's message", err)
	}
}

// Success with the recipient suppressed is a sign-in link that never comes.
func TestCloudflareReportsASuppressedRecipient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"errors":[],"result":{"queued":[],"permanent_bounces":[],"suppressed_recipients":["someone@example.test"]}}`))
	}))
	defer srv.Close()

	m, _ := NewCloudflare("acct", "tok", "a@example.test")
	m.(*cloudflareMailer).endpoint = srv.URL
	if err := m.Send(context.Background(), Message{To: "someone@example.test", Subject: "Sign in", TextBody: "link"}); err == nil {
		t.Error("Send reported success for a suppressed recipient")
	}
}
