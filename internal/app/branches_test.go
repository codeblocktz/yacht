package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// A repository URL is a tenant's input and the engine runs inside the
// cluster, so a remote on a private address is refused before anything is
// sent: otherwise the branch picker would be a way to probe the cluster's own
// network and read what answered from the error or the time it took. The
// server listens on loopback, which stands in for any private address.
func TestARemoteOnAPrivateAddressIsNotAsked(t *testing.T) {
	hit := make(chan string, 4)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hit <- r.URL.String()
	}))
	t.Cleanup(srv.Close)

	s := &Service{}
	for _, repo := range []string{
		srv.URL + "/acme/web.git",
		"https://localhost:" + srv.URL[strings.LastIndex(srv.URL, ":")+1:] + "/acme/web.git",
		"ssh://git@127.0.0.1/acme/web.git",
		"ssh://git@[::1]:22/acme/web.git",
	} {
		_, err := s.Branches(context.Background(), repo, "")
		if !errors.Is(err, ErrRepoUnreachable) || !strings.Contains(err.Error(), "private network") {
			t.Errorf("Branches(%s) = %v, want ErrRepoUnreachable for a private network", repo, err)
		}
		_, err = s.DefaultBranch(context.Background(), repo)
		if !errors.Is(err, ErrRepoUnreachable) || !strings.Contains(err.Error(), "private network") {
			t.Errorf("DefaultBranch(%s) = %v, want ErrRepoUnreachable for a private network", repo, err)
		}
	}
	select {
	case u := <-hit:
		t.Errorf("the private remote was asked for %s", u)
	default:
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
