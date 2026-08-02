package app

import (
	"strings"
	"testing"
)

// A repository is cloned and then built and then run, so what is accepted here
// decides what this cluster executes.
func TestARepositoryIsRefusedWhenItIsNotOne(t *testing.T) {
	for _, tc := range []struct {
		repo Repo
		why  string
	}{
		{Repo{URL: "github.com/you/app"}, "no scheme"},
		{Repo{URL: "http://github.com/you/app"}, "plain http"},
		{Repo{URL: "file:///etc/passwd"}, "a local path"},
		{Repo{URL: "git://github.com/you/app"}, "an unauthenticated protocol"},
		{Repo{URL: "https:///no-host"}, "no host"},
		{Repo{URL: "https://github.com/you/app", Branch: "--upload-pack=sh"}, "a flag as a branch"},
		{Repo{URL: "https://github.com/you/app", Branch: "a b"}, "a space in the branch"},
		{Repo{URL: "https://github.com/you/app", Subdir: "../../etc"}, "traversal"},
		{Repo{URL: "https://github.com/you/app", Subdir: "a/../../b"}, "traversal in the middle"},
		{Repo{URL: "https://github.com/you/app", Subdir: "/absolute"}, "an absolute path"},
		{Repo{URL: "https://github.com/you/app", Subdir: `back\slash`}, "a backslash"},
	} {
		if err := tc.repo.Validate(); err == nil {
			t.Errorf("accepted %+v (%s)", tc.repo, tc.why)
		}
	}

	for _, repo := range []Repo{
		{URL: "https://github.com/you/app"},
		{URL: "https://github.com/you/app.git", Branch: "main"},
		{URL: "ssh://git@github.com/you/app.git", Branch: "release/1.2"},
		{URL: "https://gitlab.example.com/you/app", Subdir: "services/api"},
	} {
		if err := repo.Validate(); err != nil {
			t.Errorf("refused %+v: %v", repo, err)
		}
	}
}

// Plain HTTP gets its own reason.
//
// It is the one refusal here somebody will think is pedantic, so the message
// has to say what the risk actually is rather than restating the rule.
func TestPlainHTTPSaysWhy(t *testing.T) {
	err := Repo{URL: "http://github.com/you/app"}.Validate()
	if err == nil {
		t.Fatal("plain http was accepted")
	}
	if !strings.Contains(err.Error(), "replaced") {
		t.Errorf("the message does not say what the risk is: %v", err)
	}
}

// An empty repository is not an invalid one.
//
// Most apps are an image and have no repository. Validate is called on all of
// them, so a zero Repo has to pass.
func TestAnAppWithNoRepositoryValidates(t *testing.T) {
	if err := (Repo{}).Validate(); err != nil {
		t.Errorf("an app with no repository was refused: %v", err)
	}
	if (Repo{}).Set() {
		t.Error("an empty repository reports itself as set")
	}
}

// A branch nobody chose is the default one.
func TestTheRefFallsBackToTheDefaultBranch(t *testing.T) {
	if got := (Repo{URL: "https://x/y"}).Ref(); got != DefaultBranch {
		t.Errorf("Ref() = %q, want %q", got, DefaultBranch)
	}
	if got := (Repo{URL: "https://x/y", Branch: "next"}).Ref(); got != "next" {
		t.Errorf("Ref() = %q, want next", got)
	}
}

// Normalising happens before validating, or a trailing slash is a refusal.
func TestNormaliseTrimsWhatAPersonTyped(t *testing.T) {
	got := Repo{
		URL: "  https://github.com/you/app  ", Branch: " main ", Subdir: " /services/api/ ",
	}.Normalise()

	if got.URL != "https://github.com/you/app" || got.Branch != "main" {
		t.Errorf("not trimmed: %+v", got)
	}
	if got.Subdir != "services/api" {
		t.Errorf("subdir = %q, want services/api", got.Subdir)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("a normalised repository was refused: %v", err)
	}
}
