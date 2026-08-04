package app

import (
	"context"
	"errors"
	"os"
	"testing"
)

// The branch a person wants is almost always one of two or three conventional
// names, and alphabetical order buries main under a hundred dependabot
// branches.
func TestBranchesAreRankedByWhatPeopleWant(t *testing.T) {
	got := []string{
		"zebra", "dependabot/npm/lodash", "main", "feature/x",
		"staging", "develop", "master", "abc",
	}
	sortBranches(got)

	want := []string{
		"main", "master", "develop", "staging",
		"abc", "dependabot/npm/lodash", "feature/x", "zebra",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// A remote's own error is often a full HTTP transcript, and on a private
// repository it is an auth challenge. Neither belongs beside a text field.
func TestARemoteErrorIsCutToOneLine(t *testing.T) {
	long := errors.New("first line\nsecond line\nthird line")
	if got := shortRemoteError(long); got != "first line" {
		t.Errorf("shortRemoteError = %q, want only the first line", got)
	}
}

// An empty URL is not an error — it is a Git app that has not been connected
// to anything yet, and the picker simply has nothing to offer.
func TestNoRepositoryIsNotAFailure(t *testing.T) {
	got, err := (&Service{}).Branches(context.Background(), "  ", "")
	if err != nil {
		t.Fatalf("Branches with no URL: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("branches = %v, want none", got)
	}
}

// Reads a real remote over the network.
//
// Opt-in for the reason the database tests are: it needs something this
// machine may not have. Skipping reads as a pass, so set YACHT_LIVE_GIT when
// changing anything in this file.
func TestBranchesAgainstARealRemote(t *testing.T) {
	if os.Getenv("YACHT_LIVE_GIT") == "" {
		t.Skip("set YACHT_LIVE_GIT to read a real remote")
	}
	s := &Service{}
	const repo = "https://github.com/go-git/go-git"

	all, err := s.Branches(context.Background(), repo, "")
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no branches came back")
	}
	if all[0] != "main" && all[0] != "master" {
		t.Errorf("first branch = %q, want a conventional one first", all[0])
	}

	// Filtering is what a picker does as somebody types.
	filtered, err := s.Branches(context.Background(), repo, "mast")
	if err != nil {
		t.Fatalf("filtered: %v", err)
	}
	for _, b := range filtered {
		if !contains(b, "mast") {
			t.Errorf("%q does not match the query", b)
		}
	}

	// A repository that does not exist and a private one are the same answer
	// from outside, and neither is worth a red field.
	_, err = s.Branches(context.Background(),
		"https://github.com/codeblocktz/definitely-not-a-real-repository", "")
	if !errors.Is(err, ErrRepoUnreachable) {
		t.Errorf("missing repo = %v, want ErrRepoUnreachable", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
