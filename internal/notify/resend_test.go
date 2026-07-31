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

func TestNewResendRejectsIncompleteConfig(t *testing.T) {
	if _, err := NewResend("", "yacht@example.test"); err == nil {
		t.Error("NewResend accepted an empty API key")
	}
	if _, err := NewResend("re_123", ""); err == nil {
		t.Error("NewResend accepted an empty from address")
	}
}

func TestResendPostsTheMessage(t *testing.T) {
	var gotAuth string
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"abc"}`))
	}))
	defer srv.Close()

	m, err := NewResend("re_123", "yacht@example.test")
	if err != nil {
		t.Fatalf("NewResend: %v", err)
	}
	m.(*resendMailer).endpoint = srv.URL

	if err := m.Send(context.Background(), Message{
		To: "someone@example.test", Subject: "Sign in", TextBody: "link",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotAuth != "Bearer re_123" {
		t.Errorf("Authorization = %q, want Bearer re_123", gotAuth)
	}
	if body["subject"] != "Sign in" {
		t.Errorf("subject = %v", body["subject"])
	}
}

// A failed send must say so. Swallowing it means a sign-in link that never
// arrives and nothing anywhere explaining why.
func TestResendReportsAnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"invalid from address"}`))
	}))
	defer srv.Close()

	m, _ := NewResend("re_123", "yacht@example.test")
	m.(*resendMailer).endpoint = srv.URL

	err := m.Send(context.Background(), Message{
		To: "someone@example.test", Subject: "Sign in", TextBody: "link",
	})
	if err == nil {
		t.Fatal("Send reported success on a 422")
	}
	if !strings.Contains(err.Error(), "invalid from address") {
		t.Errorf("error should carry the API's message, got: %v", err)
	}
}
