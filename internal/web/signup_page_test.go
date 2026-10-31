package web

import (
	"encoding/json"
	stdhtml "html"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/codeblocktz/yacht/internal/turnstile"
)

// With sign-up open, /sign-up is a page of its own: a title saying what it
// is for, one address, one button, and the way back to signing in. The
// website's address comes through ?email=, as it does on the sign-in page.
func TestSignUpPageWhenOpen(t *testing.T) {
	h := openSignupServer(t, &fakeAccounts{}, &fakeMailer{})
	rec := get(t, h, "/sign-up?email=amina%40example.test")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sign-up = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Create your account",
		DefaultSignUpLead,
		`method="post" action="/sign-up"`,
		`value="amina@example.test"`,
		`<button class="btn btn-primary" type="submit">Sign up with email</button>`,
		stdhtml.EscapeString(DefaultSignUpNext),
		"Already have an account?",
		`href="/sign-in">Sign in</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the sign-up page lacks %q", want)
		}
	}
}

// A product says its own words on the page.
func TestSignUpPageSaysTheInstallsWords(t *testing.T) {
	h := testServer(t, Options{
		Accounts: &fakeAccounts{}, Mailer: &fakeMailer{}, BaseURL: "https://yacht.test",
		BootstrapTeamID: "web-signin", OpenSignup: true,
		SignUpLead: "Deploy from Git. Pay in shillings.",
		SignUpNext: "We'll email you a link. Then you'll verify your phone.",
	})
	body := get(t, h, "/sign-up").Body.String()
	for _, want := range []string{"Deploy from Git. Pay in shillings.", "Then you&#39;ll verify your phone."} {
		if !strings.Contains(body, want) {
			t.Errorf("the sign-up page lacks %q", want)
		}
	}
	if strings.Contains(body, DefaultSignUpLead) {
		t.Error("the engine's own line is drawn beside the install's")
	}
}

// Closed, /sign-up sends people where the install says accounts are asked
// for — its request-access page, with the address — or to the sign-in page
// that says accounts are by invitation. A post is turned away the same way,
// and mails nobody.
func TestSignUpPageWhenClosed(t *testing.T) {
	for _, tc := range []struct {
		name, requestAccess, path, want string
	}{
		{"request access", "https://yacht.test/request-access", "/sign-up?email=amina%40example.test",
			"https://yacht.test/request-access?email=amina%40example.test"},
		{"request access, no address", "https://yacht.test/request-access", "/sign-up",
			"https://yacht.test/request-access"},
		{"sign in", "", "/sign-up", "/sign-in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mailer := &fakeMailer{}
			h := closedSignupServer(t, mailer, tc.requestAccess)
			rec := get(t, h, tc.path)
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != tc.want {
				t.Fatalf("GET %s = %d to %q, want 303 to %q", tc.path, rec.Code, rec.Header().Get("Location"), tc.want)
			}
			post := postForm(h, "/sign-up", url.Values{"email": {"new@example.test"}})
			if post.Code != http.StatusSeeOther {
				t.Errorf("POST /sign-up with sign-up closed = %d, want 303", post.Code)
			}
			if n := len(mailer.messages()); n != 0 {
				t.Errorf("a closed sign-up page sent %d mails", n)
			}
		})
	}
}

// A new address is mailed the link that makes its account; one that has an
// account is mailed its sign-in link. The visitor cannot tell which: the same
// status, the same page, the same headers.
func TestSignUpFormAnswersEveryAddressAlike(t *testing.T) {
	mailer := &fakeMailer{}
	h := openSignupServer(t, &fakeAccounts{}, mailer)

	registered := postForm(h, "/sign-up", url.Values{"email": {"known@example.test"}})
	stranger := postForm(h, "/sign-up", url.Values{"email": {"new@example.test"}})

	if registered.Code != http.StatusOK || stranger.Code != http.StatusOK {
		t.Fatalf("status: registered = %d, new = %d, want 200 both", registered.Code, stranger.Code)
	}
	if registered.Body.String() != stranger.Body.String() {
		t.Fatal("the page differs between an address with an account and a new one")
	}
	if !maps.EqualFunc(registered.Result().Header, stranger.Result().Header, slices.Equal) {
		t.Fatal("the headers differ between an address with an account and a new one")
	}
	body := stranger.Body.String()
	if !strings.Contains(body, "Check your mail") || !strings.Contains(body, `href="/sign-up"`) {
		t.Errorf("the answer is not Check your mail, back to the sign-up page:\n%s", body)
	}

	sent := mailer.messages()
	if len(sent) != 2 {
		t.Fatalf("mail sent = %d, want one to each address", len(sent))
	}
	if sent[0].To != "known@example.test" || !strings.Contains(sent[0].TextBody, "https://yacht.test/auth/") ||
		strings.Contains(sent[0].TextBody, "/sign-up/") {
		t.Errorf("the address with an account was not mailed its sign-in link: %+v", sent[0])
	}
	if sent[1].To != "new@example.test" || !strings.Contains(sent[1].TextBody, "https://yacht.test/sign-up/") {
		t.Errorf("the new address was not mailed a sign-up link: %+v", sent[1])
	}
}

// The sign-up form refuses what the sign-in page's link form refuses, on its
// own page: a throwaway mailbox, and a form the bot check did not pass.
func TestSignUpFormRefusesDisposableAddresses(t *testing.T) {
	mailer := &fakeMailer{}
	h := openSignupServer(t, &fakeAccounts{}, mailer)
	rec := postForm(h, "/sign-up", url.Values{"email": {"bot@mailinator.com"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "disposable email address") || !strings.Contains(body, "Create your account") {
		t.Errorf("the sign-up page does not say why:\n%s", body)
	}
	if n := len(mailer.messages()); n != 0 {
		t.Errorf("mail sent to a disposable address: %d", n)
	}
}

func TestSignUpFormChecksTheToken(t *testing.T) {
	for _, tc := range []struct {
		name, secret, token string
		want                int
		says                string
	}{
		{"pass", turnstile.TestSecretPass, turnstile.TestToken, http.StatusOK, "Check your mail"},
		{"fail", turnstile.TestSecretFail, turnstile.TestToken, http.StatusForbidden, "did not pass"},
		{"missing", turnstile.TestSecretPass, "", http.StatusForbidden, "Complete the check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			mailer := &fakeMailer{}
			h := checkedServer(t, tc.secret, &calls, mailer)
			form := url.Values{"email": {"new@example.test"}}
			if tc.token != "" {
				form.Set(turnstile.ResponseField, tc.token)
			}
			rec := postForm(h, "/sign-up", form)
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.says) {
				t.Fatalf("status = %d, want %d saying %q\n%s", rec.Code, tc.want, tc.says, rec.Body)
			}
			if mailed := len(mailer.messages()) > 0; mailed != (tc.want == http.StatusOK) {
				t.Errorf("mailed = %v", mailed)
			}
			if tc.want != http.StatusOK && !strings.Contains(rec.Body.String(), "Create your account") {
				t.Error("a refused sign-up is not answered on the sign-up page")
			}
		})
	}
}

// The sign-in page points somebody new at the sign-up page when sign-up is
// open, and no longer tells them the link form makes accounts; closed, it
// says nothing of signing up.
func TestSignInPageLinksToSignUpOnlyWhenOpen(t *testing.T) {
	open := get(t, openSignupServer(t, &fakeAccounts{}, &fakeMailer{}), "/sign-in").Body.String()
	if !strings.Contains(open, "Don't have an account?") || !strings.Contains(open, `href="/sign-up">Sign up</a>`) {
		t.Error("the sign-in page does not offer sign-up while it is open")
	}
	for _, gone := range []string{"New here?", "creates your account"} {
		if strings.Contains(open, gone) {
			t.Errorf("the sign-in page still says %q", gone)
		}
	}
	closed := get(t, closedSignupServer(t, &fakeMailer{}, "https://yacht.test/request-access"), "/sign-in").Body.String()
	if strings.Contains(closed, `href="/sign-up"`) || strings.Contains(closed, "Sign up") {
		t.Error("the sign-in page offers sign-up on an install where it is closed")
	}
}

// One widget on each page. On the sign-up page it sits in the one form; on
// the sign-in page it sits outside both forms, each of which carries a field
// for its token. The policy lets Cloudflare in on both pages.
func TestOneBotCheckPerPage(t *testing.T) {
	var calls int
	h := checkedServer(t, turnstile.TestSecretPass, &calls, &fakeMailer{})
	for _, path := range []string{"/sign-in", "/sign-up"} {
		rec := get(t, h, path)
		doc, err := html.Parse(strings.NewReader(rec.Body.String()))
		if err != nil {
			t.Fatal(err)
		}
		var widgets, inForm, tokenFields, forms int
		var walk func(n *html.Node, form bool)
		walk = func(n *html.Node, form bool) {
			if n.Type == html.ElementNode {
				switch {
				case n.Data == "form":
					forms++
					form = true
				case slices.Contains(strings.Fields(attr(n, "class")), "cf-turnstile"):
					widgets++
					if form {
						inForm++
					}
				case n.Data == "input" && attr(n, "name") == turnstile.ResponseField:
					tokenFields++
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, form)
			}
		}
		walk(doc, false)
		if widgets != 1 {
			t.Errorf("%s: %d cf-turnstile elements, want 1", path, widgets)
		}
		switch path {
		case "/sign-in":
			if inForm != 0 || forms != 2 || tokenFields != 2 {
				t.Errorf("/sign-in: widget in a form = %d, forms = %d, token fields = %d; want 0, 2, 2",
					inForm, forms, tokenFields)
			}
		case "/sign-up":
			if inForm != 1 || tokenFields != 0 {
				t.Errorf("/sign-up: widget in the form = %d, token fields = %d; want 1, 0", inForm, tokenFields)
			}
		}
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-src 'self' "+turnstile.Origin) {
			t.Errorf("%s: the policy keeps the widget out", path)
		}
		if strings.Count(rec.Body.String(), turnstile.ScriptURL) != 1 {
			t.Errorf("%s: Cloudflare's script is not on the page once", path)
		}
	}
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// The sign-in page's one token goes with whichever form is sent. This runs
// the script the dashboard serves against the forms the page renders, with
// the widget's passed check played by the harness, then posts each form as
// the browser would: both are let through. Before the check has passed,
// neither form carries a token and the server refuses both.
func TestTheSharedTokenGoesWithEitherForm(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is not on PATH, and CI must run the token script")
		}
		t.Skip("node is not on PATH")
	}
	var calls int
	h := checkedServer(t, turnstile.TestSecretPass, &calls, &fakeMailer{})
	doc, err := html.Parse(strings.NewReader(get(t, h, "/sign-in").Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := json.Marshal(formTree(doc))
	if err != nil {
		t.Fatal(err)
	}
	treeFile := filepath.Join(t.TempDir(), "page.json")
	if err := os.WriteFile(treeFile, tree, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{"passed", turnstile.TestToken, 0},
		{"not passed", "", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.Command(node, "testdata/turnstile_share_harness.js",
				"assets/js/turnstile-share.js", treeFile, tc.token).CombinedOutput()
			if err != nil {
				t.Fatalf("the harness failed: %v\n%s", err, out)
			}
			var forms []struct {
				Action string
				Fields map[string]string
			}
			if err := json.Unmarshal(out, &forms); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if len(forms) != 2 {
				t.Fatalf("forms = %d, want 2: %s", len(forms), out)
			}
			for _, f := range forms {
				if got := f.Fields[turnstile.ResponseField]; got != tc.token {
					t.Errorf("%s sends token %q, want %q", f.Action, got, tc.token)
				}
				post := url.Values{}
				for k, v := range f.Fields {
					post.Set(k, v)
				}
				post.Set("email", "known@example.test")
				if _, ok := f.Fields["password"]; ok {
					post.Set("password", "not-the-password")
				}
				rec := postForm(h, f.Action, post)
				refused := rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "Complete the check")
				if tc.want == http.StatusForbidden && !refused {
					t.Errorf("%s with no token = %d, want refused for the check", f.Action, rec.Code)
				}
				if tc.want == 0 && refused {
					t.Errorf("%s with the shared token was refused for the check", f.Action)
				}
			}
		})
	}
}

// formTree is the page cut down to what turnstile-share.js and the harness
// look at: forms, inputs, and anything marked data-turnstile*, nested as
// they were.
func formTree(n *html.Node) []*dialogNode {
	var out []*dialogNode
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		kids := formTree(c)
		if c.Type != html.ElementNode || !(c.Data == "form" || c.Data == "input" || turnstileMarked(c)) {
			out = append(out, kids...)
			continue
		}
		el := &dialogNode{Tag: c.Data, Attrs: map[string]string{}, Children: kids}
		for _, a := range c.Attr {
			el.Attrs[a.Key] = a.Val
		}
		out = append(out, el)
	}
	return out
}

func turnstileMarked(n *html.Node) bool {
	for _, a := range n.Attr {
		if strings.HasPrefix(a.Key, "data-turnstile") {
			return true
		}
	}
	return false
}
