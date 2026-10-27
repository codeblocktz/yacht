package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/dbrelease"
)

// DatabaseReleases says where a running PostgreSQL version stands.
// *dbrelease.Postgres is one.
type DatabaseReleases interface {
	Report(ctx context.Context, running dbrelease.Version) (dbrelease.Report, error)
}

// DatabaseRelease is where a database's version stands: what it runs, what
// its makers have released since, and whether its image's tag now names a
// newer image than the one running.
type DatabaseRelease struct {
	// Engine is the database's name as its makers write it, and Version
	// what the server says it is: "PostgreSQL", "16.4". Version is empty
	// when the database could not be asked.
	Engine  string
	Version string

	// Report is PostgreSQL's releases and advisories measured against
	// Version; Known is false when they could not be read.
	Report dbrelease.Report
	Known  bool

	// NewerImage says the image's tag names a different image from the one
	// running, so a redeploy pulls it. Pinned says the tag names this exact
	// release — postgres:16.4-alpine — so no redeploy will move it, and
	// Suggested is the tag that would: postgres:16.15-alpine.
	NewerImage bool
	Pinned     bool
	Suggested  string
}

// Attention says somebody should do something: a security fix the database
// is without, or a major version that no longer gets them.
func (r DatabaseRelease) Attention() bool {
	if !r.Known {
		return false
	}
	return len(r.Report.Fixes) > 0 || !r.Report.Supported
}

// tagTTL is how long what a tag names is believed. Docker Hub limits
// anonymous asks, and an official image's tag moves a few times a month.
const tagTTL = 6 * time.Hour

type tagDigest struct {
	digest string
	at     time.Time
}

// DatabaseRelease reads a database's version from the database itself and
// measures it against what its makers have released. PostgreSQL only: it is
// the one engine whose makers publish both releases and advisories in a form
// that can be read.
//
// Nothing here fails the page it is shown on. A database that cannot be
// asked has no version; a site that cannot be read leaves Known false.
func (s *Service) DatabaseRelease(ctx context.Context, ownerID, name string) (DatabaseRelease, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return DatabaseRelease{}, err
	}
	if a.Source != SourcePostgres {
		return DatabaseRelease{}, ErrNotADatabase
	}
	out := DatabaseRelease{Engine: "PostgreSQL"}
	if a.ActiveReleaseID == nil {
		return out, nil
	}
	release := *a.ActiveReleaseID

	out.Version = s.runningVersion(ctx, ownerID, name, release)
	running, err := dbrelease.ParseVersion(out.Version)
	if err != nil {
		return out, nil //nolint:nilerr // a version not learned is shown as none
	}
	out.Version = running.String()

	if s.opts.DatabaseReleases != nil {
		if r, err := s.opts.DatabaseReleases.Report(ctx, running); err == nil {
			out.Report, out.Known = r, true
		} else {
			s.log.Debug("database releases unreadable", "app", name, "err", err)
		}
	}
	if !out.Report.Behind() {
		return out, nil
	}
	if suggested, ok := suggestedTag(a.Image, running, out.Report.Latest); ok {
		out.Pinned, out.Suggested = true, suggested
		return out, nil
	}
	if rel, err := s.ReleaseByID(ctx, ownerID, a.ID, release); err == nil && rel.ImageDigest != "" {
		if now, ok := s.tagDigest(ctx, a.Image); ok && now != rel.ImageDigest {
			out.NewerImage = true
		}
	}
	return out, nil
}

// runningVersion is what the database says it is, asked once per release:
// a version cannot change without the image changing, and the image cannot
// change without a release.
func (s *Service) runningVersion(ctx context.Context, ownerID, name string, release uuid.UUID) string {
	s.dbVersionsMu.Lock()
	v, ok := s.dbVersions[release]
	s.dbVersionsMu.Unlock()
	if ok {
		return v
	}
	b, err := s.DatabaseBrowser(ctx, ownerID, name)
	if err != nil {
		return ""
	}
	st, err := b.Stats(ctx)
	if err != nil || st.Version == "" {
		return ""
	}
	s.dbVersionsMu.Lock()
	if s.dbVersions == nil {
		s.dbVersions = map[uuid.UUID]string{}
	}
	s.dbVersions[release] = st.Version
	s.dbVersionsMu.Unlock()
	return st.Version
}

// tagDigest is the digest an image's tag names now, kept for tagTTL.
func (s *Service) tagDigest(ctx context.Context, image string) (string, bool) {
	if s.manifests == nil || strings.Contains(image, "@") {
		return "", false
	}
	s.dbVersionsMu.Lock()
	c, ok := s.tagDigests[image]
	s.dbVersionsMu.Unlock()
	if ok && time.Since(c.at) < tagTTL {
		return c.digest, true
	}
	digest, err := s.manifests.ResolveDigest(ctx, image)
	if err != nil {
		s.log.Debug("database image tag unreadable", "image", image, "err", err)
		return "", false
	}
	s.dbVersionsMu.Lock()
	if s.tagDigests == nil {
		s.tagDigests = map[string]tagDigest{}
	}
	s.tagDigests[image] = tagDigest{digest: digest, at: time.Now()}
	s.dbVersionsMu.Unlock()
	return digest, true
}

// suggestedTag is the image to change to when the image's tag names the
// running release exactly — "16.4", "16.4-alpine", "16.4-bookworm" — rather
// than the newest of a major version as "16-alpine" does: the same tag with
// the latest release in it, "16.15-alpine".
func suggestedTag(image string, running, latest dbrelease.Version) (string, bool) {
	if strings.Contains(image, "@") {
		return "", false
	}
	i := strings.LastIndex(image, ":")
	if i < 0 || strings.Contains(image[i:], "/") {
		return "", false
	}
	tag, v := image[i+1:], running.String()
	if tag != v && !strings.HasPrefix(tag, v+"-") {
		return "", false
	}
	return image[:i+1] + latest.String() + strings.TrimPrefix(tag, v), true
}
