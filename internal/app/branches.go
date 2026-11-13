package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/codeblocktz/yacht/internal/egress"
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

	refs, err := s.listRefs(ctx, repoURL)
	if err != nil {
		// Deliberately not wrapped with the remote's own message. It is often
		// a full HTTP transcript, and on a private repository it is an auth
		// challenge — neither belongs on a page beside a text field.
		if errors.Is(err, transport.ErrAuthenticationRequired) ||
			errors.Is(err, transport.ErrAuthorizationFailed) {
			return nil, fmt.Errorf("%w: it is private, or does not exist", ErrRepoUnreachable)
		}
		return nil, remoteError(err)
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

// remoteError is a failed ls-remote as ErrRepoUnreachable. A private address
// is said as such, because it is the one failure that waiting will not fix.
func remoteError(err error) error {
	if errors.Is(err, errSSHListing) {
		return fmt.Errorf("%w: %w", ErrRepoUnreachable, err)
	}
	if errors.Is(err, egress.ErrBlocked) {
		return fmt.Errorf("%w: its host is on a private network, which is not reached from here",
			ErrRepoUnreachable)
	}
	return fmt.Errorf("%w: %s", ErrRepoUnreachable, shortRemoteError(err))
}

// listRefs is ls-remote, held to public addresses.
//
// The URL is a tenant's input and the engine runs inside the cluster, so
// without a guard "https://10.43.0.1/x" would have it probe the cluster's API
// and report, in the error or in how long it took, what answered. The check
// is in the dial, through go-git's HTTP transport given a client of the
// engine's own rather than its process-wide default: every redirect hop is
// checked, and a name that resolves publicly when looked at and privately when
// connected to is still refused. What the remote answers is read up to
// maxAdvertisement and no further.
//
// HTTPS only. Over SSH there is nothing to list with: the engine has no key
// to offer, and every real host refuses an ls-remote without one. Connecting
// anyway would be the engine dialling whatever host was typed — through
// go-git's SSH transport, which takes no context and sets no deadline on its
// handshake, so a host that accepts and says nothing holds the request
// forever. So an SSH URL is answered without a connection.
func (s *Service) listRefs(ctx context.Context, repoURL string) ([]*plumbing.Reference, error) {
	ep, err := transport.NewEndpoint(repoURL)
	if err != nil {
		return nil, err
	}
	switch ep.Protocol {
	case "https":
	case "ssh":
		return nil, errSSHListing
	default:
		return nil, fmt.Errorf("app: %q is not a scheme this can read — https or ssh", ep.Protocol)
	}
	guarded, err := s.opts.Outbound.Transport(gitHTTPBase)
	if err != nil {
		return nil, err
	}
	client := githttp.NewClient(&http.Client{Transport: cappedBodies{guarded, maxAdvertisement}})
	session, err := client.NewUploadPackSession(ep, nil)
	if err != nil {
		return nil, err
	}
	defer session.Close() //nolint:errcheck
	adv, err := session.AdvertisedReferencesContext(ctx)
	if err != nil {
		return nil, err
	}
	all, err := adv.AllReferences()
	if err != nil {
		return nil, err
	}
	refs := make([]*plumbing.Reference, 0, len(all))
	for _, ref := range all {
		refs = append(refs, ref)
	}
	return refs, nil
}

// errSSHListing is an SSH repository's branches, which are not listed.
var errSSHListing = errors.New("branches are not listed over SSH — use an https:// URL, or connect the repository through the GitHub App")

// gitHTTPBase is the transport ls-remote's guarded one is made from; nil is
// net/http's default. A variable so a test can trust its own TLS server.
var gitHTTPBase http.RoundTripper

// maxAdvertisement bounds what a remote's ref advertisement is read to. A
// repository with tens of thousands of branches and tags advertises a few
// megabytes; past this, it is a remote answering with something else.
const maxAdvertisement = 16 << 20

// errAdvertisementTooLarge is a remote that kept answering past
// maxAdvertisement.
var errAdvertisementTooLarge = errors.New("the remote's list of branches is larger than is read")

// cappedBodies stops reading a response body at max bytes, with an error
// rather than a silent end, so a cut list is never taken for a whole one.
type cappedBodies struct {
	rt  http.RoundTripper
	max int64
}

func (c cappedBodies) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &cappedBody{ReadCloser: resp.Body, left: c.max}
	return resp, nil
}

type cappedBody struct {
	io.ReadCloser
	left int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errAdvertisementTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, err
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

	refs, err := s.listRefs(ctx, strings.TrimSpace(repoURL))
	if err != nil {
		return "", remoteError(err)
	}

	for _, ref := range refs {
		if ref.Name() == plumbing.HEAD && ref.Type() == plumbing.SymbolicReference {
			return ref.Target().Short(), nil
		}
	}
	return "", nil
}
