package turnstile

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// standIn answers siteverify as Cloudflare documents its test secrets to:
// the passing secret takes any token, the failing one none, and the third
// says the token was already spent. It also records what it was sent.
func standIn(t *testing.T, got *[]map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse: %v", err)
		}
		*got = append(*got, map[string]string{
			"secret": r.PostForm.Get("secret"), "response": r.PostForm.Get("response"),
			"remoteip": r.PostForm.Get("remoteip"),
		})
		answer := map[string]any{"success": false, "error-codes": []string{"invalid-input-secret"}}
		switch r.PostForm.Get("secret") {
		case TestSecretPass:
			answer = map[string]any{"success": true, "error-codes": []string{}}
		case TestSecretFail:
			answer["error-codes"] = []string{"invalid-input-response"}
		case TestSecretSpent:
			answer["error-codes"] = []string{"timeout-or-duplicate"}
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVerify(t *testing.T) {
	var got []map[string]string
	srv := standIn(t, &got)
	for _, tc := range []struct {
		name, secret, token string
		want                error
	}{
		{"pass", TestSecretPass, TestToken, nil},
		{"fail", TestSecretFail, TestToken, ErrRejected},
		{"spent", TestSecretSpent, TestToken, ErrRejected},
		{"missing", TestSecretPass, "", ErrMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := &Verifier{SiteKey: TestSiteKeyPass, Secret: tc.secret, Endpoint: srv.URL}
			err := v.Verify(context.Background(), tc.token, "198.51.100.7")
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("Verify = %v, want %v", err, tc.want)
			}
		})
	}
	// A missing token is refused without asking Cloudflare.
	if len(got) != 3 {
		t.Fatalf("siteverify asked %d times, want 3", len(got))
	}
	if got[0]["response"] != TestToken || got[0]["remoteip"] != "198.51.100.7" {
		t.Errorf("sent %+v, want the token and the visitor's address", got[0])
	}
}

// Cloudflare unreachable refuses the form rather than waving it through.
func TestVerifyFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	v := &Verifier{SiteKey: TestSiteKeyPass, Secret: TestSecretPass, Endpoint: srv.URL}
	err := v.Verify(context.Background(), TestToken, "")
	if err == nil || errors.Is(err, ErrRejected) || errors.Is(err, ErrMissing) {
		t.Fatalf("Verify = %v, want a transport error", err)
	}
	if !strings.Contains(Message(err), "could not be completed") {
		t.Errorf("message %q", Message(err))
	}
}

// Without both keys the check is off, and lets everything through.
func TestOffWithoutKeys(t *testing.T) {
	for _, v := range []*Verifier{nil, New("", TestSecretPass), New(TestSiteKeyPass, "")} {
		if v.Enabled() || v.Key() != "" {
			t.Errorf("%+v is enabled", v)
		}
		if err := v.Verify(context.Background(), "", ""); err != nil {
			t.Errorf("an unset verifier refused: %v", err)
		}
	}
}

func TestAllowWidget(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; form-action 'self'")
	AllowWidget(h)
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{
		"script-src 'self' 'unsafe-inline' " + Origin,
		"frame-src 'self' " + Origin,
		"form-action 'self'",
		"default-src 'self'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	// Twice is the same as once.
	AllowWidget(h)
	if h.Get("Content-Security-Policy") != csp {
		t.Errorf("a second call changed it: %q", h.Get("Content-Security-Policy"))
	}
	empty := http.Header{}
	AllowWidget(empty)
	if empty.Get("Content-Security-Policy") != "" {
		t.Error("a response with no policy was given one")
	}
}
