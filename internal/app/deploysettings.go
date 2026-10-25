package app

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// restartRetriesFor is the retry count stored for a policy: the one given for
// on-failure, and none for a policy that has no use for one.
func restartRetriesFor(policy orchestrator.RestartPolicy, retries int32) int32 {
	if policy == "" || policy == orchestrator.RestartOnFailure {
		return retries
	}
	return 0
}

// maxDockerfilePath is the longest Dockerfile path the store keeps.
const maxDockerfilePath = 255

// validateDockerfilePath refuses a Dockerfile path the build could not honour
// as written: absolute, or reaching out of the repository.
//
// Relative to the subdirectory a build runs in, because that is the build's
// context and where every relative path in the Dockerfile is read from; a path
// that climbed out of it would name a file the build cannot COPY beside.
func validateDockerfilePath(p string) error {
	p = strings.TrimSpace(p)
	switch {
	case p == "":
		return nil
	case len(p) > maxDockerfilePath:
		return fmt.Errorf("the Dockerfile path must be at most %d characters", maxDockerfilePath)
	case strings.HasPrefix(p, "/"):
		return errors.New("the Dockerfile path is relative to the repository, so it cannot start with /")
	case path.Clean(p) == ".." || strings.HasPrefix(path.Clean(p), "../"):
		return errors.New("the Dockerfile path must stay inside the repository")
	case strings.HasSuffix(p, "/"):
		return errors.New("the Dockerfile path names a file, not a directory")
	}
	return nil
}

// cleanDockerfilePath is a valid path as it is stored: trimmed and cleaned, so
// "./Dockerfile" and "Dockerfile" are one setting, and the default is empty.
func cleanDockerfilePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return path.Clean(p)
}

// maxWatchPaths bounds the patterns an app may have: enough for any layout
// worth describing, few enough that a pasted file list is refused rather than
// matched against every push.
const maxWatchPaths = 50

// validateWatchPaths checks watch paths before they are stored.
func validateWatchPaths(patterns []string) error {
	cleaned := cleanWatchPaths(patterns)
	if len(cleaned) > maxWatchPaths {
		return fmt.Errorf("at most %d watch paths", maxWatchPaths)
	}
	for _, p := range cleaned {
		if len(p) > 255 {
			return fmt.Errorf("the watch path %q is longer than 255 characters", p[:40]+"…")
		}
	}
	// Negations alone match nothing, and so would skip every push: almost
	// certainly not what somebody who typed "!docs/**" meant.
	if len(cleaned) > 0 && allNegated(cleaned) {
		return errors.New("watch paths that only exclude match nothing — add what to watch, such as **, before the ! lines")
	}
	return nil
}

func allNegated(patterns []string) bool {
	for _, p := range patterns {
		if !strings.HasPrefix(p, "!") {
			return false
		}
	}
	return true
}

// cleanWatchPaths is patterns as they are stored: one per entry, trimmed,
// with blank lines and # comments gone, as a .gitignore would read them.
func cleanWatchPaths(patterns []string) []string {
	out := []string{}
	for _, p := range patterns {
		for _, line := range strings.Split(p, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			out = append(out, line)
		}
	}
	return out
}

// watchedChange reports whether any of files matches the app's watch paths.
//
// Gitignore semantics, through go-git's own matcher, from the repository's
// root — not the app's subdirectory — as Railway reads them: "/backend/**"
// is one directory, "*.go" is Go anywhere, and a "!" line takes back what a
// line above it matched. The last pattern to match a file decides it.
func watchedChange(patterns, files []string) bool {
	var ps []gitignore.Pattern
	for _, p := range patterns {
		ps = append(ps, gitignore.ParsePattern(p, nil))
	}
	m := gitignore.NewMatcher(ps)
	for _, f := range files {
		f = strings.Trim(f, "/")
		if f != "" && m.Match(strings.Split(f, "/"), false) {
			return true
		}
	}
	return false
}
