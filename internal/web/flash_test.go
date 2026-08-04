package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// flashServer is a server with only what a flash needs.
func flashServer(t *testing.T) *Server {
	t.Helper()
	key, err := newFlashKey()
	if err != nil {
		t.Fatalf("flash key: %v", err)
	}
	return &Server{flashKey: key}
}

// setAndTake runs one flash through a redirect the way a browser would: the
// handler sets a cookie, the browser sends it back on the next request.
func setAndTake(t *testing.T, s *Server, kind FlashKind, text string) (*Flash, *httptest.ResponseRecorder) {
	t.Helper()

	post := httptest.NewRecorder()
	s.setFlash(post, httptest.NewRequest(http.MethodPost, "/apps/web/scale", nil), kind, text)

	next := httptest.NewRequest(http.MethodGet, "/apps/web", nil)
	for _, c := range post.Result().Cookies() {
		next.AddCookie(c)
	}
	get := httptest.NewRecorder()
	return s.takeFlash(get, next), get
}

func TestAFlashSurvivesTheRedirectItWasSetOn(t *testing.T) {
	s := flashServer(t)

	got, _ := setAndTake(t, s, FlashOK, "Scaled to 3 replicas.")
	if got == nil {
		t.Fatal("no flash came back")
	}
	if got.Text != "Scaled to 3 replicas." {
		t.Errorf("text = %q", got.Text)
	}
	if got.Kind != FlashOK {
		t.Errorf("kind = %q, want ok", got.Kind)
	}
}

// The whole point of a flash: it is shown once. A message that persisted would
// reappear on every page until it expired, attached to pages that have nothing
// to do with the action that produced it.
func TestAFlashIsShownOnlyOnce(t *testing.T) {
	s := flashServer(t)

	got, rec := setAndTake(t, s, FlashOK, "Domain added.")
	if got == nil {
		t.Fatal("no flash came back")
	}

	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == FlashCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("reading a flash did not clear its cookie")
	}

	// A second request with no cookie, which is what the browser now sends.
	again := s.takeFlash(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/apps/web", nil))
	if again != nil {
		t.Fatalf("flash came back a second time: %+v", again)
	}
}

// A planted cookie must not be able to choose what the dashboard says. "Your
// session expired, sign in again at …" is a convincing thing to read on a page
// that otherwise looks exactly right.
func TestATamperedFlashIsRefused(t *testing.T) {
	s := flashServer(t)

	post := httptest.NewRecorder()
	s.setFlash(post, httptest.NewRequest(http.MethodPost, "/apps", nil), FlashOK, "Deployed.")

	var raw string
	for _, c := range post.Result().Cookies() {
		if c.Name == FlashCookie {
			raw = c.Value
		}
	}
	if raw == "" {
		t.Fatal("no flash cookie was set")
	}

	tag, _, ok := strings.Cut(raw, ".")
	if !ok {
		t.Fatalf("cookie has no tag separator: %q", raw)
	}

	// Same signature, different message.
	forged := tag + "." + encodeFlashPayload(string(FlashErr)+"|Sign in again at evil.example.com")

	req := httptest.NewRequest(http.MethodGet, "/apps", nil)
	req.AddCookie(&http.Cookie{Name: FlashCookie, Value: forged})
	if got := s.takeFlash(httptest.NewRecorder(), req); got != nil {
		t.Fatalf("a forged flash was believed: %+v", got)
	}
}

// A key is minted per process, so a message signed by one server is not
// readable by another. This is the property that makes the per-process key safe
// rather than merely convenient.
func TestAFlashFromAnotherServerIsRefused(t *testing.T) {
	first, second := flashServer(t), flashServer(t)

	post := httptest.NewRecorder()
	first.setFlash(post, httptest.NewRequest(http.MethodPost, "/apps", nil), FlashOK, "Deployed.")

	req := httptest.NewRequest(http.MethodGet, "/apps", nil)
	for _, c := range post.Result().Cookies() {
		req.AddCookie(c)
	}
	if got := second.takeFlash(httptest.NewRecorder(), req); got != nil {
		t.Fatalf("another server's flash was believed: %+v", got)
	}
}

// A runaway error string must become a truncated message rather than a request
// the next server refuses for oversized headers.
func TestALongFlashIsTruncatedRatherThanRefused(t *testing.T) {
	s := flashServer(t)

	got, _ := setAndTake(t, s, FlashErr, strings.Repeat("x", maxFlashText*3))
	if got == nil {
		t.Fatal("no flash came back")
	}
	if len(got.Text) != maxFlashText {
		t.Errorf("text length = %d, want %d", len(got.Text), maxFlashText)
	}
}

// An empty message is not a message. Setting one must not leave a cookie that
// renders as an empty toast on the next page.
func TestAnEmptyFlashSetsNothing(t *testing.T) {
	s := flashServer(t)

	rec := httptest.NewRecorder()
	s.setFlash(rec, httptest.NewRequest(http.MethodPost, "/apps", nil), FlashOK, "   ")

	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("an empty flash set %d cookie(s)", len(cookies))
	}
}

// Errors interrupt; everything else waits for a pause. Asserted here because
// the distinction lives in one method and is easy to invert silently.
func TestOnlyFailuresAreAssertive(t *testing.T) {
	if !(Flash{Kind: FlashErr}).Assertive() {
		t.Error("a failure should interrupt")
	}
	for _, k := range []FlashKind{FlashOK, FlashWarn} {
		if (Flash{Kind: k}).Assertive() {
			t.Errorf("%q should not interrupt", k)
		}
	}
}
