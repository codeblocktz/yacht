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

// TagLister lists an image repository's tags. *registry.Store is one;
// asserted on Manifests, so an install whose resolver cannot list tags only
// loses the updates that need the list.
type TagLister interface {
	Tags(ctx context.Context, ref string) ([]string, error)
}

// DatabaseRelease is where a database's version stands: what it runs, what
// its makers have published since, and what the Update button would deploy.
type DatabaseRelease struct {
	// Engine is the database's name as its makers write it, and Version
	// what it runs: "PostgreSQL", "16.4". Version is empty when it could not
	// be learned.
	Engine  string
	Version string

	// Report is PostgreSQL's releases and advisories measured against
	// Version; Known is false when they could not be read, and always for
	// the other engines, whose makers publish neither in a form to read.
	Report dbrelease.Report
	Known  bool

	// Series is the line an update stays within, "16" or "8.4", and Newest
	// the newest release of it with an image published; both empty when the
	// image's tags could not be read.
	Series string
	Newest string

	// Update is what the Update button deploys, nil when there is nothing
	// newer within the series to move to.
	Update *DatabaseUpdate

	// NextMajor is a newer series than the one running, "17", which nothing
	// here moves to: a new major version reads a data directory the old one
	// wrote only after a migration, and PostgreSQL refuses to start on it at
	// all. Empty when the series running is the newest.
	NextMajor string
}

// DatabaseUpdate is a newer image of the series a database runs.
type DatabaseUpdate struct {
	// Version is the release it moves to, "16.16"; empty when the image's
	// tag names a newer build without the tags saying which release it is.
	Version string
	// Label names it for a person and for the deployment it is recorded as:
	// "PostgreSQL 16.16".
	Label string
	// Image is what deploys: the app's own image when its tag follows the
	// series and has moved, and otherwise the exact release's tag, so
	// postgres:16.4-alpine becomes postgres:16.16-alpine.
	Image string
	// Backup says a backup is taken first, and the update deploys once it
	// has finished.
	Backup bool
}

// Attention says somebody should do something: a security fix the database
// is without, or a major version that no longer gets them.
func (r DatabaseRelease) Attention() bool {
	if !r.Known {
		return false
	}
	return len(r.Report.Fixes) > 0 || !r.Report.Supported
}

// databaseEngine is what DatabaseRelease knows about one engine.
type databaseEngine struct {
	label string
	line  dbrelease.Line
}

// databaseEngines are the engines whose versions are followed. Redis is
// numbered like PostgreSQL, and moving 7.2 to 7.4 in place is what its
// makers support; MySQL's 8.0 and 8.4 are separate series, and MongoDB's
// 7.0 and 8.0 separate major versions, so both only move within X.Y.
var databaseEngines = map[Source]databaseEngine{
	SourcePostgres: {label: "PostgreSQL", line: dbrelease.Line{Series: 1, Release: 2}},
	SourceMySQL:    {label: "MySQL", line: dbrelease.Line{Series: 2, Release: 3}},
	SourceRedis:    {label: "Redis", line: dbrelease.Line{Series: 1, Release: 3}},
	SourceMongo:    {label: "MongoDB", line: dbrelease.Line{Series: 2, Release: 3}},
}

// tagTTL is how long what a tag names is believed. Docker Hub limits
// anonymous asks, and an official image's tag moves a few times a month.
const tagTTL = 6 * time.Hour

// tagListTTL is how long a repository's tags are believed, and
// tagListRetry how long one that could not be listed is left before asking
// again. A tag list is one request of up to 90 KB, asked only when somebody
// opens a database's Deployments tab.
const (
	tagListTTL   = 12 * time.Hour
	tagListRetry = 15 * time.Minute
)

type tagDigest struct {
	digest string
	at     time.Time
}

type tagList struct {
	tags []string
	ok   bool
	at   time.Time
}

// DatabaseRelease reads a database's version and measures it against what
// its makers have published: the image tags its repository has for every
// engine, and for PostgreSQL its releases and advisories as well.
//
// Nothing here fails the page it is shown on. A database that cannot be
// asked has no version; a registry or a site that cannot be read leaves
// what it would have said unsaid.
func (s *Service) DatabaseRelease(ctx context.Context, ownerID, name string) (DatabaseRelease, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return DatabaseRelease{}, err
	}
	engine, ok := databaseEngines[a.Source]
	if !ok {
		return DatabaseRelease{}, ErrNotADatabase
	}
	out := DatabaseRelease{Engine: engine.label}
	if a.ActiveReleaseID == nil {
		return out, nil
	}
	release := *a.ActiveReleaseID

	in := updatePlan{line: engine.line, image: a.Image, digest: func(ref string) (string, bool) {
		return s.tagDigest(ctx, ref)
	}}
	// What the active release runs, not what the app names: an image
	// changed and still deploying is not yet the database's version.
	if rel, err := s.ReleaseByID(ctx, ownerID, a.ID, release); err == nil {
		in.released = rel.ImageDigest
		if rel.ImageRef != "" {
			in.image = rel.ImageRef
		}
	}
	if _, asks := browsers[a.Source]; asks {
		in.running = s.runningNumbers(ctx, a, release, &out)
	}
	in.tags, _ = s.imageTags(ctx, a.Image)
	if out.Known && out.Report.Behind() {
		in.published, _ = dbrelease.ParseNumbers(out.Report.Latest.String())
	}

	plan := in.plan()
	if out.Version == "" && plan.running != nil {
		out.Version = plan.running.String()
	}
	if plan.newest != nil {
		out.Series, out.Newest = plan.series.String(), plan.newest.String()
	}
	if plan.next != nil {
		out.NextMajor = plan.next.String()
	}
	if plan.image != "" {
		u := &DatabaseUpdate{Version: plan.version.String(), Image: plan.image}
		switch {
		case u.Version == "":
			u.Label = "a newer " + a.Image + " image"
		case plan.rebuild:
			u.Label = "a newer build of " + engine.label + " " + u.Version
		default:
			u.Label = engine.label + " " + u.Version
		}
		_, _, backupErr := s.backupsFor(a)
		u.Backup = backupErr == nil
		out.Update = u
	}
	return out, nil
}

// runningNumbers is what the database says it runs. PostgreSQL's answer is
// also measured against postgresql.org, filling out's Report.
func (s *Service) runningNumbers(ctx context.Context, a App, release uuid.UUID, out *DatabaseRelease) dbrelease.Numbers {
	said := s.runningVersion(ctx, a.OwnerID, a.Name, release)
	if a.Source != SourcePostgres {
		v, _ := dbrelease.ParseNumbers(said)
		return v
	}
	running, err := dbrelease.ParseVersion(said)
	if err != nil {
		return nil
	}
	out.Version = running.String()
	if s.opts.DatabaseReleases != nil {
		if r, err := s.opts.DatabaseReleases.Report(ctx, running); err == nil {
			out.Report, out.Known = r, true
		} else {
			s.log.Debug("database releases unreadable", "app", a.Name, "err", err)
		}
	}
	v, _ := dbrelease.ParseNumbers(running.String())
	return v
}

// updatePlan is everything an update is decided from, gathered first so the
// decision itself can be tested without a registry or a database.
type updatePlan struct {
	line  dbrelease.Line
	image string
	// running is what the database says it runs, nil when it was not asked
	// or could not be.
	running dbrelease.Numbers
	// tags are the image repository's, nil when they could not be listed.
	tags []string
	// released is the digest the active release runs.
	released string
	// published is the makers' own word on the newest release of the
	// running series, PostgreSQL's, for when the tags cannot be listed.
	published dbrelease.Numbers
	// digest is what a tag names now.
	digest func(ref string) (string, bool)
}

type planned struct {
	running, series, newest, next dbrelease.Numbers
	// image is what an update deploys, "" for no update; version what it
	// moves to, nil when the tags cannot say; rebuild that it is the same
	// release built again.
	image   string
	version dbrelease.Numbers
	rebuild bool
}

func (p updatePlan) plan() planned {
	var out planned
	repo, tagName, ok := splitImage(p.image)
	if !ok {
		// Named by digest: nothing will move it but a person.
		out.running = p.running
		return out
	}
	tag, ok := dbrelease.ParseTag(tagName)
	if !ok {
		out.running = p.running
		return out
	}
	releases := dbrelease.NewReleases(p.tags, p.line, tag.Variant)

	running := p.running
	if running == nil && tag.Exact(p.line) {
		running = tag.Version
	}
	if running == nil && releases.Known() {
		// The newest release the tag follows is what it ran, if its image is
		// the one running.
		if v, ok := releases.Newest(tag.Version); ok && p.released != "" {
			if d, ok := p.digest(repo + ":" + dbrelease.TagFor(v, tag.Variant)); ok && d == p.released {
				running = v
			}
		}
	}
	out.running = running

	if running != nil {
		out.series = p.line.SeriesOf(running)
		out.newest, _ = releases.Newest(out.series)
		out.next, _ = releases.NextSeries(out.series)
		if out.newest == nil && p.published.HasPrefix(out.series) && len(out.series) > 0 {
			out.newest = p.published
		}
	}
	// A tag that follows the running series moves within it, so pulling it
	// again is the update. One that follows more — mysql:8 across 8.0 and
	// 8.4, or latest — could move the major version, and is pinned instead.
	follows := tag.Follows(p.line) && (running == nil || running.HasPrefix(tag.Version))
	moved := ""
	if follows && p.released != "" {
		if d, ok := p.digest(p.image); ok && d != p.released {
			moved = d
		}
	}

	switch {
	case running != nil && out.newest != nil && out.newest.Compare(running) > 0:
		if follows {
			if moved != "" {
				out.image, out.version = p.image, out.newest
			}
			return out
		}
		target := repo + ":" + dbrelease.TagFor(out.newest, tag.Variant)
		if _, ok := p.digest(target); ok {
			out.image, out.version = target, out.newest
		}
	case moved != "":
		// The same release rebuilt — a new base image under it — or a tag
		// whose release the database cannot be asked.
		out.image, out.version = p.image, running
		out.rebuild = running != nil
		if running == nil {
			if v, ok := releases.Newest(tag.Version); ok {
				if d, ok := p.digest(repo + ":" + dbrelease.TagFor(v, tag.Variant)); ok && d == moved {
					out.version = v
				}
			}
		}
	}
	return out
}

// splitImage is an image's repository and tag: "postgres", "16-alpine".
// A missing tag is "latest", as a pull reads it; false for an image named
// by digest.
func splitImage(image string) (repo, tag string, ok bool) {
	if strings.Contains(image, "@") {
		return "", "", false
	}
	i := strings.LastIndex(image, ":")
	if i < 0 || strings.Contains(image[i:], "/") {
		return image, "latest", true
	}
	return image[:i], image[i+1:], true
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

// imageTags is the tags of an image's repository, kept for tagListTTL; one
// that could not be listed is not asked again for tagListRetry, so a
// registry that cannot be reached is waited on once rather than per page.
func (s *Service) imageTags(ctx context.Context, image string) ([]string, bool) {
	lister, ok := s.manifests.(TagLister)
	repo, _, split := splitImage(image)
	if !ok || !split {
		return nil, false
	}
	s.dbVersionsMu.Lock()
	c, cached := s.tagLists[repo]
	s.dbVersionsMu.Unlock()
	if cached && (c.ok && time.Since(c.at) < tagListTTL || !c.ok && time.Since(c.at) < tagListRetry) {
		return c.tags, c.ok
	}
	tags, err := lister.Tags(ctx, repo)
	if err != nil {
		s.log.Debug("database image tags unreadable", "image", image, "err", err)
	}
	s.dbVersionsMu.Lock()
	if s.tagLists == nil {
		s.tagLists = map[string]tagList{}
	}
	s.tagLists[repo] = tagList{tags: tags, ok: err == nil, at: time.Now()}
	s.dbVersionsMu.Unlock()
	return tags, err == nil
}
