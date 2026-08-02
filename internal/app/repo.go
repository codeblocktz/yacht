package app

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// DefaultBranch is what a repository is built from when nobody says.
//
// A default rather than a required field: almost every repository uses it, and
// a form that refuses to submit without a value everyone would type the same
// way is a form asking a question it already knows the answer to.
const DefaultBranch = "main"

// Repo is where an app's source comes from.
//
// A value type rather than three columns threaded through every signature,
// because the three only ever travel together and only ever mean anything
// together: a subdirectory with no URL is not a partial configuration, it is a
// mistake.
type Repo struct {
	// URL is an https:// or ssh:// Git URL.
	URL string

	// Branch is the ref built. Empty means DefaultBranch.
	Branch string

	// Subdir is the directory inside the repository to build from, for a
	// monorepo. Empty is the root.
	Subdir string
}

// Set reports whether this app builds from source.
func (r Repo) Set() bool { return r.URL != "" }

// Ref is the branch actually used.
func (r Repo) Ref() string {
	if r.Branch == "" {
		return DefaultBranch
	}
	return r.Branch
}

// A branch, tag, or anything else `git clone --branch` accepts. Refusing the
// rest is not about Git's own rules: this value is passed to a clone inside a
// build container, so the set is narrowed to what cannot be read as anything
// but a ref.
var refRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// A monorepo path. No leading slash, no traversal, no backslashes — the value
// is joined onto a checkout directory inside the builder, and ".." there walks
// out of the repository into the build container's filesystem.
var subdirRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// Validate checks a repository somebody typed.
func (r Repo) Validate() error {
	if !r.Set() {
		return nil
	}

	u, err := url.Parse(r.URL)
	if err != nil {
		return fmt.Errorf("app: %q is not a URL", r.URL)
	}
	switch u.Scheme {
	case "https", "ssh":
	case "http":
		// Refused rather than upgraded. A repository fetched over plain HTTP
		// can be replaced in transit by anyone on the path, and what comes back
		// is built and run in this cluster.
		return errors.New("app: use https rather than http — " +
			"a repository fetched in the clear can be replaced on the way here, " +
			"and what arrives gets built and run")
	case "":
		return errors.New("app: a repository URL needs a scheme, e.g. " +
			"https://github.com/you/app.git")
	default:
		return fmt.Errorf("app: %q is not a scheme this can clone — https or ssh", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("app: %q has no host", r.URL)
	}

	if r.Branch != "" && !refRE.MatchString(r.Branch) {
		return fmt.Errorf("app: %q is not a branch name", r.Branch)
	}
	if r.Subdir != "" {
		if !subdirRE.MatchString(r.Subdir) || strings.Contains(r.Subdir, "..") {
			return fmt.Errorf("app: %q is not a path inside the repository", r.Subdir)
		}
	}
	return nil
}

// Normalise trims a repository the way it will be stored.
func (r Repo) Normalise() Repo {
	return Repo{
		URL:    strings.TrimSpace(r.URL),
		Branch: strings.TrimSpace(r.Branch),
		Subdir: strings.Trim(strings.TrimSpace(r.Subdir), "/"),
	}
}
