package web

import (
	"regexp"
	"testing"
)

var titleTag = regexp.MustCompile(`<title>([^<]*)</title>`)

// Every signed-in page was titled with the product's name alone, so browser
// tabs, history and a screen reader announcing the page could not tell any
// two apart. A page is titled with where it is, most specific first, then
// the product.
func TestPagesAreTitledWithWhereTheyAre(t *testing.T) {
	h := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "web"))})

	for path, want := range map[string]string{
		"/":               "Projects · Yacht",
		"/admin":          "Overview · Admin · Yacht",
		"/cluster/pods":   "Cluster · Admin · Yacht",
		"/cluster/events": "Events · Admin · Yacht",
		"/settings":       "Settings · Admin · Yacht",
		// The canvas with the app's panel open: the app, then its project.
		"/apps/web": "web · " + fakeProject.Name + " · Apps · Yacht",
	} {
		m := titleTag.FindStringSubmatch(get(t, h, path).Body.String())
		if m == nil {
			t.Errorf("%s has no title", path)
			continue
		}
		if m[1] != want {
			t.Errorf("%s is titled %q, want %q", path, m[1], want)
		}
	}
}

// A signed-out page names itself, and has no trail to be titled from.
func TestASignedOutPageKeepsItsOwnTitle(t *testing.T) {
	h := testServer(t, Options{
		Accounts: &fakeAccounts{}, Mailer: &fakeMailer{}, BaseURL: "https://yacht.test",
		BootstrapTeamID: "owner-1",
	})
	m := titleTag.FindStringSubmatch(get(t, h, "/sign-in").Body.String())
	if m == nil || m[1] != "Sign in" {
		t.Errorf("the sign-in page is titled %v, want \"Sign in\"", m)
	}
}
