package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
)

// Listing the directories in a repository, so the root directory is chosen
// rather than typed.
//
// Branches come from ls-remote, which is part of git and works against any
// host. A directory listing is not: git has no way to read one tree entry
// without fetching objects, so this asks the host's own API instead. That means
// GitHub only, which is stated rather than hidden — a field that silently stops
// helping on GitLab would be worse than one that says why.
//
// Public repositories need no credential. GITHUB_TOKEN is used when set, which
// is what a private repository and a busy install both need: unauthenticated
// requests are limited to sixty an hour per address.

// ErrTreeUnsupported means the host is not one whose tree can be read.
var ErrTreeUnsupported = fmt.Errorf("app: only GitHub repositories can be browsed")

// maxEntries caps a listing. A directory with a thousand entries is not
// something to scroll; it is something to type a path into.
const maxEntries = 100

// Directories lists the directories at a path inside a repository.
//
// path is relative to the repository root and empty means the root itself,
// matching what the build does with SUBDIR. Returns only directories: a file
// cannot be a root directory, and offering one would be offering a build that
// fails on the first step.
func (s *Service) Directories(ctx context.Context, repoURL, path string) ([]string, error) {
	owner, repo, ok := gitHubRepo(repoURL)
	if !ok {
		return nil, ErrTreeUnsupported
	}

	path = strings.Trim(strings.TrimSpace(path), "/")
	if strings.Contains(path, "..") {
		return nil, fmt.Errorf("app: %q is not a path inside the repository", path)
	}

	ctx, cancel := context.WithTimeout(ctx, branchTimeout)
	defer cancel()

	endpoint := "https://api.github.com/repos/" + url.PathEscape(owner) + "/" +
		url.PathEscape(repo) + "/contents"
	if path != "" {
		endpoint += "/" + path
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrRepoUnreachable, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	// Optional. Set it for a private repository, or to lift the sixty-an-hour
	// limit that unauthenticated requests share across everything on this
	// address.
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrRepoUnreachable, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
		// A private repository, a repository that does not exist and a rate
		// limit are the same answer from outside: this cannot be read right
		// now. None of them is worth a red field beside a text input.
		return nil, fmt.Errorf("%w: it is private, missing, or rate-limited", ErrRepoUnreachable)
	default:
		return nil, fmt.Errorf("%w: GitHub answered %s", ErrRepoUnreachable, resp.Status)
	}

	// A path that names a file answers with an object rather than an array.
	// Decoding into a slice fails there, which is the right answer — a file has
	// no directories in it.
	var entries []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrRepoUnreachable, err)
	}

	var dirs []string
	for _, e := range entries {
		if e.Type != "dir" {
			continue
		}
		// Dotted directories are build machinery — .github, .git, .devcontainer
		// — and never what somebody is pointing a build at.
		if strings.HasPrefix(e.Name, ".") {
			continue
		}
		dirs = append(dirs, e.Name)
	}

	sort.Strings(dirs)
	if len(dirs) > maxEntries {
		dirs = dirs[:maxEntries]
	}
	return dirs, nil
}

// gitHubRepo pulls the owner and repository out of a URL.
//
// Handles the forms people paste: with and without .git, with and without a
// trailing slash, https and ssh. Anything else is not GitHub as far as this is
// concerned, which the caller reports rather than guessing at.
func gitHubRepo(repoURL string) (owner, repo string, ok bool) {
	s := strings.TrimSpace(repoURL)
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")

	switch {
	case strings.HasPrefix(s, "git@github.com:"):
		s = strings.TrimPrefix(s, "git@github.com:")
	case strings.HasPrefix(s, "https://github.com/"):
		s = strings.TrimPrefix(s, "https://github.com/")
	case strings.HasPrefix(s, "http://github.com/"):
		s = strings.TrimPrefix(s, "http://github.com/")
	default:
		return "", "", false
	}

	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
