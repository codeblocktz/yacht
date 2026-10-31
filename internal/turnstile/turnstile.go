// Package turnstile checks that a form was sent by a person: Cloudflare
// Turnstile's widget, drawn on the page, hands the browser a token, and the
// server asks Cloudflare's siteverify whether the token is good before it
// acts on the form.
//
// A token is worth one check and five minutes. It says nothing about who
// the person is — only that a bot did not fill in the form — so it stands
// beside the other checks a sign-up passes, never instead of one.
package turnstile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// Origin is where the widget's script and its frames come from: the one
	// origin a page drawing it must let in, in script-src and frame-src.
	Origin = "https://challenges.cloudflare.com"
	// ScriptURL is the widget, rendering every element of class cf-turnstile.
	ScriptURL = Origin + "/turnstile/v0/api.js"
	// SiteverifyURL is where a token is checked.
	SiteverifyURL = Origin + "/turnstile/v0/siteverify"
	// ResponseField is the form field the widget puts its token in.
	ResponseField = "cf-turnstile-response"
)

// Cloudflare's documented test keys
// (developers.cloudflare.com/turnstile/troubleshooting/testing/): a site key
// whose widget always passes, or always fails, and secrets whose siteverify
// always answers yes, no, or that the token was spent. The passing widget
// hands out TestToken, which a real secret refuses.
const (
	TestSiteKeyPass = "1x00000000000000000000AA"
	TestSiteKeyFail = "2x00000000000000000000AB"
	TestSecretPass  = "1x0000000000000000000000000000000AA"
	TestSecretFail  = "2x0000000000000000000000000000000AA"
	TestSecretSpent = "3x0000000000000000000000000000000AA"
	TestToken       = "XXXX.DUMMY.TOKEN.XXXX"
)

var (
	// ErrMissing is a form sent without a token: the widget never ran, which
	// is a script blocked, or a client that is not a browser.
	ErrMissing = errors.New("turnstile: no token")
	// ErrRejected is a token siteverify refused: failed, spent or expired.
	ErrRejected = errors.New("turnstile: token refused")
)

// Verifier checks tokens with one site's secret. A nil Verifier, an install
// with no keys, is off: Enabled is false and Verify lets everything through.
type Verifier struct {
	// SiteKey is public, drawn into the page; Secret is the server's.
	SiteKey string
	Secret  string
	// Endpoint is siteverify, SiteverifyURL unless a test points it at a
	// stand-in.
	Endpoint string
	// Client makes the call, with a short timeout unless it says otherwise.
	Client *http.Client
}

// New is a Verifier for a site, or nil when either key is missing.
func New(siteKey, secret string) *Verifier {
	siteKey, secret = strings.TrimSpace(siteKey), strings.TrimSpace(secret)
	if siteKey == "" || secret == "" {
		return nil
	}
	return &Verifier{SiteKey: siteKey, Secret: secret}
}

// Enabled reports whether forms are checked at all.
func (v *Verifier) Enabled() bool { return v != nil && v.SiteKey != "" && v.Secret != "" }

// Key is the site key to draw the widget with, or "" when off.
func (v *Verifier) Key() string {
	if !v.Enabled() {
		return ""
	}
	return v.SiteKey
}

// siteverifyAnswer is the part of Cloudflare's answer acted on.
type siteverifyAnswer struct {
	Success    bool     `json:"success"`
	ErrorCodes []string `json:"error-codes"`
}

// Verify asks siteverify about a token, giving the visitor's address as
// Cloudflare recommends. Nil is a person. ErrMissing and ErrRejected are the
// visitor's; any other error is siteverify unreachable or answering
// nonsense, and the form is refused then too: a check that lets everything
// through whenever Cloudflare is slow is no check.
func (v *Verifier) Verify(ctx context.Context, token, remoteIP string) error {
	if !v.Enabled() {
		return nil
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return ErrMissing
	}
	// A token is at most 2048 characters; anything longer is not one, and
	// is not sent on.
	if len(token) > 2048 {
		return ErrRejected
	}
	form := url.Values{"secret": {v.Secret}, "response": {token}}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	endpoint := v.Endpoint
	if endpoint == "" {
		endpoint = SiteverifyURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("turnstile: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("turnstile: siteverify: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("turnstile: siteverify answered %s", resp.Status)
	}
	var a siteverifyAnswer
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&a); err != nil {
		return fmt.Errorf("turnstile: siteverify: %w", err)
	}
	if !a.Success {
		return fmt.Errorf("%w: %s", ErrRejected, strings.Join(a.ErrorCodes, ", "))
	}
	return nil
}

// VerifyRequest is Verify with the token from a posted form and the address
// the request came from.
func (v *Verifier) VerifyRequest(r *http.Request, remoteIP string) error {
	if !v.Enabled() {
		return nil
	}
	return v.Verify(r.Context(), r.PostFormValue(ResponseField), remoteIP)
}

// Message is what a person is told when the check refused their form.
func Message(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrMissing):
		return "Complete the check that you are a person, then send the form again. " +
			"If no check appears, allow scripts from challenges.cloudflare.com."
	case errors.Is(err, ErrRejected):
		return "The check that you are a person did not pass. Try it again."
	default:
		return "The check that you are a person could not be completed just now. Try again in a minute."
	}
}

// AllowWidget widens a response's Content-Security-Policy to let the widget
// in — its script, and the frame it draws the challenge in — and nothing
// else. For the pages that draw it only: every other page keeps the policy
// that lets no third party in at all. A response with no policy is left
// with none.
func AllowWidget(h http.Header) {
	csp := h.Get("Content-Security-Policy")
	if csp == "" {
		return
	}
	parts := strings.Split(csp, ";")
	var script, frame bool
	for i, p := range parts {
		p = strings.TrimSpace(p)
		switch {
		case strings.HasPrefix(p, "script-src ") || p == "script-src":
			if !strings.Contains(p, Origin) {
				p += " " + Origin
			}
			script = true
		case strings.HasPrefix(p, "frame-src ") || p == "frame-src":
			if !strings.Contains(p, Origin) {
				p += " " + Origin
			}
			frame = true
		}
		parts[i] = p
	}
	// Without its own directive each falls back to default-src, 'self',
	// which keeps Cloudflare out.
	if !script {
		parts = append(parts, "script-src 'self' "+Origin)
	}
	if !frame {
		parts = append(parts, "frame-src 'self' "+Origin)
	}
	h.Set("Content-Security-Policy", strings.Join(parts, "; "))
}
