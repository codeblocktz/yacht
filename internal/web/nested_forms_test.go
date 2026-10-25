package web

import (
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/codeblocktz/yacht/internal/app"
)

// nestedForms finds forms inside forms. A browser ignores the inner start
// tag and lets its end tag close the outer form, so every field after it
// belongs to no form: it is drawn, and never sent.
func nestedForms(t *testing.T, page string) []string {
	t.Helper()
	z := html.NewTokenizer(strings.NewReader(page))
	var open []string
	var nested []string
	for {
		switch z.Next() {
		case html.ErrorToken:
			return nested
		case html.StartTagToken:
			name, hasAttr := z.TagName()
			if string(name) != "form" {
				continue
			}
			action := ""
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				if string(k) == "action" {
					action = string(v)
				}
			}
			if len(open) > 0 {
				nested = append(nested, action+" inside "+open[len(open)-1])
			}
			open = append(open, action)
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "form" && len(open) > 0 {
				open = open[:len(open)-1]
			}
		}
	}
}

// A Git app's settings, with a repository connected and so the source
// card's Disconnect, are one form whose fields all reach it.
func TestTheSettingsPageHasNoFormInsideAForm(t *testing.T) {
	a := sampleApp("owner-1", "web")
	a.Source = app.SourceGit
	a.Repo = app.Repo{URL: "https://github.com/acme/web.git", Branch: "main"}
	h := testServer(t, Options{Apps: newFakeApps(a)})

	body := get(t, h, "/apps/web/settings").Body.String()
	if nested := nestedForms(t, body); len(nested) > 0 {
		t.Errorf("forms inside forms: %v", nested)
	}
	for _, want := range []string{`form="source-disconnect"`, `id="source-disconnect"`, `name="port"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the settings page does not have %q", want)
		}
	}
}
