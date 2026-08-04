package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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

// TestNoHandRolledFormControls.
//
// The design system ships a checkbox and a switch, and both were being
// reimplemented by hand — a native input with local classes, which is a second
// implementation of a component that already exists and the one that does not
// inherit its focus ring, its disabled treatment or its dark mode.
//
// Checked against the templates rather than a rendered page, because the
// question is about every control that exists rather than the ones some test
// happened to render.
func TestNoHandRolledFormControls(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(".", "*.templ"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates found: %v", err)
	}

	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(src), `type="checkbox"`) {
			t.Errorf("%s writes a native checkbox — use checkbox.Checkbox, "+
				"through checkField or checkOption", filepath.Base(f))
		}
		if strings.Contains(string(src), `type="radio"`) {
			t.Errorf("%s writes a native radio — templUI ships one", filepath.Base(f))
		}
	}
}

// A checkbox's value is whatever the markup happened to say, and nothing
// should depend on which.
//
// Two handlers compared against "on" — the value a native checkbox submits
// when the markup gives it none — so both silently read false the moment the
// control started sending "1". Enforcing HTTPS quietly stopped working, and
// nothing anywhere said so.
func TestACheckboxIsReadByPresenceNotByValue(t *testing.T) {
	for _, value := range []string{"1", "on", "true", "anything"} {
		form := url.Values{"liveness": {value}}
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		if !formChecked(req, "liveness") {
			t.Errorf("a box submitting %q reads as unchecked", value)
		}
	}

	// Absent is the only unchecked. A browser omits the field entirely.
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if formChecked(req, "liveness") {
		t.Error("an absent box reads as checked")
	}
}

// And nothing goes back to comparing one against a literal, which is what let
// the control and the handler disagree.
//
// The names come from the templates rather than from a list here, so a control
// added tomorrow is covered without anybody remembering to add it. Fields that
// are not checkboxes are left alone on purpose: the cordon form posts a hidden
// unschedulable=true/false from two buttons, where the value genuinely is the
// signal and comparing against it is correct.
func TestNoHandlerComparesACheckboxAgainstALiteral(t *testing.T) {
	control := regexp.MustCompile(`@(?:checkField|checkOption|toggle)\("([a-z_]+)"`)

	names := map[string]bool{}
	templates, _ := filepath.Glob(filepath.Join(".", "*.templ"))
	for _, f := range templates {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range control.FindAllStringSubmatch(string(src), -1) {
			names[m[1]] = true
		}
	}
	if len(names) == 0 {
		t.Fatal("found no checkbox controls to check the handlers against")
	}

	sources, _ := filepath.Glob(filepath.Join(".", "*.go"))
	for _, f := range sources {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for name := range names {
			bad := regexp.MustCompile(`FormValue\("` + name + `"\)\s*(==|!=)\s*"`)
			for _, m := range bad.FindAllString(string(src), -1) {
				t.Errorf("%s reads the %q checkbox by value (%s) — use formChecked",
					filepath.Base(f), name, strings.TrimSpace(m))
			}
		}
	}
}
