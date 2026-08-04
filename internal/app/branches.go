package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"

	gogit "github.com/go-git/go-git/v5"
)

// Reading a repository's branches without cloning it.
//
// Yacht has never read a repository. The only thing that touches one is
// `git clone --depth 1` inside the build Job, which happens minutes after
// somebody has already typed a branch name and long after the moment they
// could have been helped.
//
// ls-remote is the cheap half of git: one HTTPS request, no working tree, no
// disk. It is enough to answer "which branches exist", which is the question
// somebody is guessing at when they type into that field.

// ErrRepoUnreachable means the remote did not answer, or answered by refusing.
//
// Its own error because the page treats it as "cannot help right now" rather
// than as the repository being wrong: a private repository, a typo and an
// outage all land here, and only one of them is worth a red field.
var ErrRepoUnreachable = errors.New("app: the repository could not be read")

// branchTimeout bounds the lookup.
//
// It runs while somebody is typing, so it has to fail faster than they lose
// patience. A remote that has not answered in this long is one the picker
// gives up on, leaving the field usable as free text.
const branchTimeout = 6 * time.Second

// maxBranches caps what is returned.
//
// A long-lived repository can have thousands of branches, and a picker is not
// a way to read all of them — it is a way to find one. Filtering happens
// before this cap so a search still reaches a branch that sorts late.
const maxBranches = 50

// Branches lists a repository's branches, most useful first.
//
// query filters by substring, which is what a picker does as somebody types.
// Anonymous over HTTPS: that covers public repositories, and a private one
// fails as ErrRepoUnreachable rather than prompting for a credential this
// install has nowhere to keep.
func (s *Service) Branches(ctx context.Context, repoURL, query string) ([]string, error) {
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		return nil, nil
	}
	if err := (Repo{URL: repoURL}).Validate(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, branchTimeout)
	defer cancel()

	remote := gogit.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin",
		URLs: []string{repoURL},
	})

	refs, err := remote.ListContext(ctx, &gogit.ListOptions{})
	if err != nil {
		// Deliberately not wrapped with the remote's own message. It is often
		// a full HTTP transcript, and on a private repository it is an auth
		// challenge — neither belongs on a page beside a text field.
		if errors.Is(err, transport.ErrAuthenticationRequired) ||
			errors.Is(err, transport.ErrAuthorizationFailed) {
			return nil, fmt.Errorf("%w: it is private, or does not exist", ErrRepoUnreachable)
		}
		return nil, fmt.Errorf("%w: %s", ErrRepoUnreachable, shortRemoteError(err))
	}

	needle := strings.ToLower(strings.TrimSpace(query))
	var names []string
	for _, ref := range refs {
		if !ref.Name().IsBranch() {
			continue
		}
		name := ref.Name().Short()
		if needle != "" && !strings.Contains(strings.ToLower(name), needle) {
			continue
		}
		names = append(names, name)
	}

	sortBranches(names)
	if len(names) > maxBranches {
		names = names[:maxBranches]
	}
	return names, nil
}

// sortBranches puts the ones somebody is most likely to want at the top.
//
// Alphabetical order buries main under a hundred dependabot branches, and the
// branch a person wants is almost always one of two or three conventional
// names.
func sortBranches(names []string) {
	rank := func(n string) int {
		switch n {
		case "main", "master":
			return 0
		case "develop", "development", "dev":
			return 1
		case "staging", "production", "prod":
			return 2
		}
		return 3
	}
	sort.SliceStable(names, func(i, j int) bool {
		if ri, rj := rank(names[i]), rank(names[j]); ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
}

// shortRemoteError keeps the first line and nothing else.
func shortRemoteError(err error) string {
	msg, _, _ := strings.Cut(err.Error(), "\n")
	if len(msg) > 120 {
		msg = msg[:120]
	}
	return msg
}

// DefaultBranch is what the remote itself points HEAD at.
//
// Better than assuming "main": a repository that predates the rename still
// says master, and picking wrong sends somebody to a build that clones an
// empty ref.
func (s *Service) DefaultBranch(ctx context.Context, repoURL string) (string, error) {
	if err := (Repo{URL: strings.TrimSpace(repoURL)}).Validate(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, branchTimeout)
	defer cancel()

	remote := gogit.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin", URLs: []string{strings.TrimSpace(repoURL)},
	})
	refs, err := remote.ListContext(ctx, &gogit.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrRepoUnreachable, shortRemoteError(err))
	}

	for _, ref := range refs {
		if ref.Name() == plumbing.HEAD && ref.Type() == plumbing.SymbolicReference {
			return ref.Target().Short(), nil
		}
	}
	return "", nil
}
