package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/turnstile"
)

// fakeSiteverify answers as Cloudflare documents its test secrets to: the
// passing secret takes any token, the failing one none. It counts its calls.
func fakeSiteverify(t *testing.T, calls *int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		_ = r.ParseForm()
		ok := r.PostForm.Get("secret") == turnstile.TestSecretPass && r.PostForm.Get("response") != ""
		_ = json.NewEncoder(w).Encode(map[string]any{"success": ok, "error-codes": []string{}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func checkedServer(t *testing.T, secret string, calls *int, mailer *fakeMailer) http.Handler {
	t.Helper()
	return testServer(t, Options{
		Accounts:        &fakeAccounts{},
		Mailer:          mailer,
		BaseURL:         "https://yacht.test",
		BootstrapTeamID: "web-signin",
		OpenSignup:      true,
		Turnstile: &turnstile.Verifier{
			SiteKey: turnstile.TestSiteKeyPass, Secret: secret, Endpoint: fakeSiteverify(t, calls),
		},
		Identity: identity.ProviderFunc(func(context.Context, *http.Request) (identity.Owner, error) {
			return identity.Owner{}, identity.ErrUnauthenticated
		}),
	})
}

func postForm(h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://yacht.test")
	r.RemoteAddr = "198.51.100.20:4000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// The page draws one widget for both forms, and only it lets Cloudflare in.
func TestSignInPageDrawsTheBotCheck(t *testing.T) {
	var calls int
	h := checkedServer(t, turnstile.TestSecretPass, &calls, &fakeMailer{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sign-in", nil))
	body := rec.Body.String()
	if n := strings.Count(body, `data-sitekey="`+turnstile.TestSiteKeyPass+`"`); n != 1 {
		t.Errorf("widgets drawn = %d, want one for both forms", n)
	}
	if !strings.Contains(body, turnstile.ScriptURL) {
		t.Error("the widget's script is not on the page")
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' 'unsafe-inline' "+turnstile.Origin) ||
		!strings.Contains(csp, "frame-src 'self' "+turnstile.Origin) {
		t.Errorf("the sign-in page's policy keeps the widget out: %s", csp)
	}

	// Any other page keeps Cloudflare out.
	other := httptest.NewRecorder()
	h.ServeHTTP(other, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if strings.Contains(other.Header().Get("Content-Security-Policy"), turnstile.Origin) {
		t.Errorf("a page without the widget lets Cloudflare in: %s", other.Header().Get("Content-Security-Policy"))
	}
}

func TestSignInLinkFormChecksTheToken(t *testing.T) {
	for _, tc := range []struct {
		name, secret, token string
		want                int
		mailed              bool
		says                string
	}{
		{"pass", turnstile.TestSecretPass, turnstile.TestToken, http.StatusOK, true, ""},
		{"fail", turnstile.TestSecretFail, turnstile.TestToken, http.StatusForbidden, false, "did not pass"},
		{"missing", turnstile.TestSecretPass, "", http.StatusForbidden, false, "Complete the check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			mailer := &fakeMailer{}
			h := checkedServer(t, tc.secret, &calls, mailer)
			form := url.Values{"email": {"new@example.test"}}
			if tc.token != "" {
				form.Set(turnstile.ResponseField, tc.token)
			}
			rec := postForm(h, "/sign-in", form)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d\n%s", rec.Code, tc.want, rec.Body)
			}
			if got := len(mailer.messages()) > 0; got != tc.mailed {
				t.Errorf("mailed = %v, want %v", got, tc.mailed)
			}
			if tc.says != "" && !strings.Contains(rec.Body.String(), tc.says) {
				t.Errorf("the page does not say %q", tc.says)
			}
			// A form without a token is refused without asking Cloudflare.
			if tc.token == "" && calls != 0 {
				t.Errorf("siteverify asked %d times for a form with no token", calls)
			}
		})
	}
}

func TestPasswordFormChecksTheToken(t *testing.T) {
	var calls int
	h := checkedServer(t, turnstile.TestSecretFail, &calls, &fakeMailer{})
	rec := postForm(h, "/sign-in/password", url.Values{
		"email": {"known@example.test"}, "password": {"whatever"},
		turnstile.ResponseField: {turnstile.TestToken},
	})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "did not pass") {
		t.Fatalf("status = %d, want 403 with the reason\n%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("siteverify asked %d times, want 1", calls)
	}
	missing := postForm(h, "/sign-in/password", url.Values{"email": {"known@example.test"}, "password": {"x"}})
	if missing.Code != http.StatusForbidden {
		t.Errorf("a password form with no token: status %d, want 403", missing.Code)
	}
}

// Open sign-up refuses a throwaway mailbox on the page, politely, and mails
// it nothing; any case and any subdomain of the domain is refused alike.
func TestOpenSignupRefusesDisposableAddresses(t *testing.T) {
	mailer := &fakeMailer{}
	h := openSignupServer(t, &fakeAccounts{}, mailer)
	for _, email := range []string{"bot@mailinator.com", "Bot@MAILINATOR.COM", "bot@x.mailinator.com"} {
		rec := postForm(h, "/sign-in", url.Values{"email": {email}})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", email, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "disposable email address") {
			t.Errorf("%s: the page does not say why", email)
		}
	}
	if n := len(mailer.messages()); n != 0 {
		t.Errorf("mail sent to disposable addresses: %d", n)
	}
	if rec := postForm(h, "/sign-in", url.Values{"email": {"person@example.test"}}); rec.Code != http.StatusOK {
		t.Errorf("an ordinary address: status = %d, want 200", rec.Code)
	}
}
