package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/secret"
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

// redirectFlash sets a message on one server and takes it on another, the way
// a POST and the GET after its redirect land on different replicas.
func redirectFlash(t *testing.T, from, to *Server) *Flash {
	t.Helper()
	post := httptest.NewRecorder()
	from.setFlash(post, httptest.NewRequest(http.MethodPost, "/apps", nil), FlashOK, "Deployed.")

	req := httptest.NewRequest(http.MethodGet, "/apps", nil)
	for _, c := range post.Result().Cookies() {
		req.AddCookie(c)
	}
	return to.takeFlash(httptest.NewRecorder(), req)
}

// A server minting its own key does not believe another's message.
func TestAFlashFromAnotherServerIsRefused(t *testing.T) {
	if got := redirectFlash(t, flashServer(t), flashServer(t)); got != nil {
		t.Fatalf("another server's flash was believed: %+v", got)
	}
}

// replicaFor is one dashboard replica built as the engine builds it, its flash
// key derived from the install's YACHT_SECRET_KEY.
func replicaFor(t *testing.T, secretKey string) *Server {
	t.Helper()
	keeper, err := secret.NewKeeper(secretKey)
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	key, err := keeper.DeriveKey(FlashKeyPurpose)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	s, err := New(Options{
		Orchestrator: orchestrator.NewNoop(),
		Identity:     identity.NewSingleOwner(identity.Owner{ID: "owner-1"}),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		FlashKey:     key,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// Two replicas of one install share the secret key, so the replica serving the
// redirect shows the message the other set (CONFIG-14: with a key per process,
// one redirect in two replicas' worth showed its toast). Another install's
// replica, with its own secret, must still refuse it.
func TestReplicasWithTheSameSecretKeyShareFlashes(t *testing.T) {
	const install = "a2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2s="
	const other = "ampqampqampqampqampqampqampqampqampqampqams="

	first, second := replicaFor(t, install), replicaFor(t, install)
	got := redirectFlash(t, first, second)
	if got == nil || got.Text != "Deployed." {
		t.Fatalf("a replica of the same install refused the flash: %+v", got)
	}
	if got := redirectFlash(t, first, replicaFor(t, other)); got != nil {
		t.Fatalf("a server with another secret key believed the flash: %+v", got)
	}
}

// A configured key too short to be one is a wiring mistake, refused at start.
func TestAShortFlashKeyIsRefused(t *testing.T) {
	_, err := New(Options{
		Orchestrator: orchestrator.NewNoop(),
		Identity:     identity.NewSingleOwner(identity.Owner{ID: "owner-1"}),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		FlashKey:     []byte("short"),
	})
	if err == nil {
		t.Fatal("a 5-byte flash key was accepted")
	}
}

// With no key configured the process mints one, and says in the log that more
// than one replica will lose messages; the log must not carry key material.
func TestNoFlashKeyWarns(t *testing.T) {
	var buf strings.Builder
	_, err := New(Options{
		Orchestrator: orchestrator.NewNoop(),
		Identity:     identity.NewSingleOwner(identity.Owner{ID: "owner-1"}),
		Logger:       slog.New(slog.NewTextHandler(&buf, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "YACHT_SECRET_KEY") {
		t.Fatalf("no warning about the unshared flash key:\n%s", buf.String())
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
