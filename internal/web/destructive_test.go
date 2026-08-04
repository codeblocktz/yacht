package web

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEveryDestructiveFormAsksFirst.
//
// The confirmation dialog exists and is generic, and nothing enforces its use.
// A form that deletes an app, destroys a volume or removes a node is one click
// from doing something that does not come back, and the only thing standing
// between a misplaced click and that outcome is somebody having remembered to
// add data-confirm to the markup.
//
// This is the remembering, written down. It reads the templates rather than the
// rendered pages because the question is about every form that exists, not the
// subset some test happened to render.
func TestEveryDestructiveFormAsksFirst(t *testing.T) {
	// Verbs whose endpoints take something away. Matched against the form's
	// action, which is the only part of a form that says what it does.
	destructive := regexp.MustCompile(`/(delete|remove|disconnect|revoke|clear)\b`)

	// Forms are matched whole so the action and the attributes are read
	// together — an action on one line and a data-confirm three lines later is
	// still one form.
	formRE := regexp.MustCompile(`(?s)<form\b.*?>`)
	actionRE := regexp.MustCompile(`action=\{?\s*templ\.(?:Safe)?URL\("([^"]*)"`)

	files, err := filepath.Glob(filepath.Join(".", "*.templ"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates found: %v", err)
	}

	var unguarded []string
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, form := range formRE.FindAllString(string(src), -1) {
			m := actionRE.FindStringSubmatch(form)
			if m == nil {
				continue
			}
			action := m[1]
			if !destructive.MatchString(action) {
				continue
			}
			// data-confirm may sit on the form or on the submit button — the
			// dialog script accepts either, and a per-button one is right when
			// a form has several submits.
			if strings.Contains(form, "data-confirm") {
				continue
			}
			unguarded = append(unguarded, filepath.Base(f)+": "+action)
		}
	}

	// Actions that take nothing away despite their names. Listed rather than
	// pattern-matched, so adding one is a decision somebody makes on purpose.
	allowed := map[string]bool{
		// Clears the arrangement of cards on a canvas. Re-arranging is what
		// the button beside it does, and nothing is lost either way.
		"/projects/" + "{slug}" + "/arrange": true,
	}

	var real []string
	for _, u := range unguarded {
		_, action, _ := strings.Cut(u, ": ")
		if !allowed[action] {
			real = append(real, u)
		}
	}

	sort.Strings(real)
	for _, u := range real {
		t.Errorf("a destructive form does not confirm first: %s", u)
	}
}

// The dialog is only a dialog. Without JavaScript the form submits directly, so
// anything that must not happen by accident needs a check the server makes too.
//
// Deleting an app and deleting a volume both ask for the name to be typed, and
// both check it server-side. This asserts the second half is really there,
// because the first half is what everybody remembers to write.
func TestIrreversibleActionsAreCheckedOnTheServer(t *testing.T) {
	for _, f := range []string{"server.go", "storage.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !strings.Contains(string(src), `FormValue("confirm")`) {
			t.Errorf("%s has no server-side confirmation check", f)
		}
	}
}
