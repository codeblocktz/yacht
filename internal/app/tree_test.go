package app

import (
	"context"
	"errors"
	"os"
	"testing"
)

// The forms people actually paste. Getting this wrong means the picker quietly
// stops working for half of them with no explanation.
func TestGitHubRepoParsing(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/codeblocktz/yacht":     "codeblocktz/yacht",
		"https://github.com/codeblocktz/yacht.git": "codeblocktz/yacht",
		"https://github.com/codeblocktz/yacht/":    "codeblocktz/yacht",
		"git@github.com:codeblocktz/yacht.git":     "codeblocktz/yacht",
		"  https://github.com/codeblocktz/yacht  ": "codeblocktz/yacht",
	} {
		owner, repo, ok := gitHubRepo(in)
		if !ok {
			t.Errorf("%q was not recognised", in)
			continue
		}
		if got := owner + "/" + repo; got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}

	// Not GitHub, and said so rather than guessed at. A field that silently
	// stops helping is worse than one that explains why.
	for _, in := range []string{
		"https://gitlab.com/a/b",
		"https://git.example.com/a/b",
		"https://github.com/only-an-owner",
		"nonsense",
		"",
	} {
		if _, _, ok := gitHubRepo(in); ok {
			t.Errorf("%q was treated as a GitHub repository", in)
		}
	}
}

// A host whose tree cannot be read is its own answer, not a lookup failure.
func TestANonGitHubRepoIsUnsupportedRatherThanBroken(t *testing.T) {
	_, err := (&Service{}).Directories(
		context.Background(), "https://gitlab.com/a/b", "")
	if !errors.Is(err, ErrTreeUnsupported) {
		t.Errorf("err = %v, want ErrTreeUnsupported", err)
	}
}

// A path that climbs out of the repository is refused here as well as in
// Repo.Validate, because this one reaches the network.
func TestATraversalPathIsRefused(t *testing.T) {
	_, err := (&Service{}).Directories(
		context.Background(), "https://github.com/a/b", "../../etc")
	if err == nil {
		t.Fatal("a traversal path was accepted")
	}
	if errors.Is(err, ErrTreeUnsupported) {
		t.Errorf("err = %v, want the path itself refused", err)
	}
}

// Reads GitHub over the network. Opt-in, and a skip reads as a pass.
func TestDirectoriesAgainstARealRepository(t *testing.T) {
	if os.Getenv("YACHT_LIVE_GIT") == "" {
		t.Skip("set YACHT_LIVE_GIT to read a real repository")
	}
	s := &Service{}
	const repo = "https://github.com/go-git/go-git"

	root, err := s.Directories(context.Background(), repo, "")
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	if len(root) == 0 {
		t.Fatal("no directories at the root")
	}
	for _, d := range root {
		if d[0] == '.' {
			t.Errorf("%q is build machinery and should not be offered", d)
		}
	}

	// Nested paths, because a root directory is often two levels down.
	sub, err := s.Directories(context.Background(), repo, "plumbing")
	if err != nil {
		t.Fatalf("nested: %v", err)
	}
	if len(sub) == 0 {
		t.Error("no directories inside plumbing")
	}

	// Missing, private and rate-limited are one answer from outside, and none
	// of them is worth a red field.
	_, err = s.Directories(context.Background(),
		"https://github.com/codeblocktz/definitely-not-a-real-repository", "")
	if !errors.Is(err, ErrRepoUnreachable) {
		t.Errorf("missing repo = %v, want ErrRepoUnreachable", err)
	}
}
