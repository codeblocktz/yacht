package web

import (
	"context"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/notify"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files under testdata/mail")

// The sign-in mail as it reaches a mailbox, text and HTML, signed the way the
// engine signs it. Wording and layout are what spam filters and people judge
// it by, so a change to either has to be a change somebody meant: run with
// -update and read the diff.
func TestSignInMailGolden(t *testing.T) {
	mailer := &fakeMailer{}
	h := testServer(t, Options{
		Accounts: &fakeAccounts{},
		Mailer: notify.WithBranding(mailer, notify.Footer{
			Brand:   "datahuru",
			Company: "hosting by Codeblock Limited, Dar es Salaam, Tanzania",
			Website: "https://datahuru.com",
		}, "hello@datahuru.com"),
		BaseURL: "https://app.datahuru.test", BootstrapTeamID: "web-signin", WakerBrand: "datahuru",
		Identity: identity.ProviderFunc(func(context.Context, *http.Request) (identity.Owner, error) {
			return identity.Owner{}, identity.ErrUnauthenticated
		}),
	})
	if code := postSignIn(h, "known@example.test", "198.51.100.7:3000").Code; code != http.StatusOK {
		t.Fatalf("POST /sign-in = %d", code)
	}
	sent := mailer.messages()
	if len(sent) != 1 {
		t.Fatalf("mail sent = %d, want 1", len(sent))
	}
	m := sent[0]
	if m.ReplyTo != "hello@datahuru.com" {
		t.Errorf("Reply-To = %q", m.ReplyTo)
	}
	golden(t, "signin.txt", "Subject: "+m.Subject+"\n\n"+m.Text())
	golden(t, "signin.html", m.HTML()+"\n")
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "mail", name)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the golden file; run with -update and read the diff.\ngot:\n%s", name, got)
	}
}
