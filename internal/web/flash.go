package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
)

// A one-shot message carried across a redirect.
//
// Every mutation in this dashboard is a form POST, and the only way to report
// what it did used to be to render a page with a notice on it. That is why a
// failed action rendered the standalone app page rather than the canvas sheet it
// was started from: the handler could not both redirect and say anything, so it
// stopped redirecting.
//
// A flash separates the two. The handler says what happened, redirects to
// wherever the person actually was, and the next page render picks the message
// up and drops it. Nothing is stored server-side, so this costs no table and
// survives no longer than the one redirect it exists for.

// FlashCookie is where a pending message waits. Not HttpOnly-exempt and not
// readable by script for any good reason, so it is HttpOnly like the session.
const FlashCookie = "yacht_flash"

// maxFlashText caps what will be carried.
//
// The text ends up in a cookie the browser sends back, and a cookie jar is a
// small budget shared with the session. Messages here are one sentence; a limit
// well above that turns a runaway error string into a truncated message rather
// than a request with headers too large to serve.
const maxFlashText = 240

// flashTTL is how long a pending message stays redeemable.
//
// A minute rather than a session: a flash describes something that just
// happened, and one that surfaced an hour later — on the next visit, attached to
// an unrelated page — would be worse than one that was lost.
const flashTTL = 60

// FlashKind decides how a message is drawn, and nothing else.
type FlashKind string

const (
	FlashOK   FlashKind = "ok"
	FlashErr  FlashKind = "err"
	FlashWarn FlashKind = "warn"
)

// Flash is a message waiting to be shown once.
type Flash struct {
	Kind FlashKind
	Text string
}

// Class maps a kind to its toast modifier. Unknown kinds draw as neutral rather
// than as nothing: a message with a mangled kind is still a message.
func (f Flash) Class() string {
	switch f.Kind {
	case FlashOK:
		return "toast-ok"
	case FlashErr:
		return "toast-err"
	case FlashWarn:
		return "toast-warn"
	}
	return ""
}

// Assertive reports whether a screen reader should interrupt for this.
//
// Only failures do. A polite live region is announced when the user pauses,
// which is right for "scaled to 3 replicas" and wrong for the one message that
// says the thing they asked for did not happen.
func (f Flash) Assertive() bool { return f.Kind == FlashErr }

// newFlashKey mints the key that signs pending messages.
//
// Random per process, deliberately. The alternative is deriving it from
// YACHT_SECRET_KEY, which would tie a purely cosmetic feature to a setting an
// install may not have — and the cost of a fresh key is that a redirect crossing
// a restart loses its toast. That is the correct thing to lose.
func newFlashKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

// encodeFlashPayload is how a message is written into the cookie.
//
// One definition rather than an inline call at each site, so the test that
// forges a payload cannot accidentally agree with itself while disagreeing with
// what the server actually writes.
func encodeFlashPayload(payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// sign returns the authentication tag for a payload.
func (s *Server) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, s.flashKey)
	mac.Write(payload)
	// Truncated: this authenticates a sentence shown to the person who caused
	// it, not a capability. Sixteen bytes is far more than that needs and keeps
	// the cookie small.
	return mac.Sum(nil)[:16]
}

// setFlash queues a message for the next page this browser renders.
//
// Signed because the text is rendered back to the person. Unsigned, anyone able
// to set a cookie on this origin could choose what the dashboard appears to say
// — "your session expired, sign in again at …" is a convincing thing to read on
// a page that otherwise looks exactly right. Signing does not stop a cookie
// being planted; it stops a planted one being believed.
func (s *Server) setFlash(w http.ResponseWriter, r *http.Request, kind FlashKind, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if len(text) > maxFlashText {
		text = text[:maxFlashText]
	}

	payload := []byte(string(kind) + "|" + text)
	value := base64.RawURLEncoding.EncodeToString(s.sign(payload)) + "." +
		encodeFlashPayload(string(payload))

	http.SetCookie(w, &http.Cookie{
		Name:     FlashCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsTLS(r),
		MaxAge:   flashTTL,
	})
}

// flashOK, flashErr and flashWarn are the three ways a handler reports an
// outcome. Named rather than passing a kind at every call site, so the grep for
// "what does this handler tell people" has three answers instead of one.
func (s *Server) flashOK(w http.ResponseWriter, r *http.Request, text string) {
	s.setFlash(w, r, FlashOK, text)
}

func (s *Server) flashErr(w http.ResponseWriter, r *http.Request, text string) {
	s.setFlash(w, r, FlashErr, text)
}

func (s *Server) flashWarn(w http.ResponseWriter, r *http.Request, text string) {
	s.setFlash(w, r, FlashWarn, text)
}

// takeFlash reads a pending message and clears it.
//
// Clearing happens whenever a cookie was present, including when it failed
// authentication. A value that does not verify is never going to start
// verifying, and leaving it would re-run this check on every page load for the
// life of the cookie.
func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) *Flash {
	c, err := r.Cookie(FlashCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	clearFlashCookie(w, r)

	tag, payload, ok := strings.Cut(c.Value, ".")
	if !ok {
		return nil
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(tag)
	if err != nil {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	if !hmac.Equal(gotMAC, s.sign(raw)) {
		return nil
	}

	kind, text, ok := strings.Cut(string(raw), "|")
	if !ok || text == "" {
		return nil
	}
	switch FlashKind(kind) {
	case FlashOK, FlashErr, FlashWarn:
	default:
		// Authentic but not a kind this version knows. Shown neutrally rather
		// than dropped: the message is genuine, and only its styling is
		// unfamiliar.
		kind = ""
	}
	if len(text) > maxFlashText {
		text = text[:maxFlashText]
	}
	return &Flash{Kind: FlashKind(kind), Text: text}
}

// clearFlashCookie expires the cookie.
//
// The attributes repeat those of setFlash for the reason clearSessionCookie
// documents: a browser only replaces a cookie whose name, path and domain
// match, so clearing with a different Path leaves the live one in place.
func clearFlashCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: FlashCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: requestIsTLS(r), MaxAge: -1,
	})
}
