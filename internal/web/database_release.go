package web

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// DatabaseReleases is a database's version and what its makers have
// released since: asked on the Deployments tab after it has drawn, since the
// answer can mean asking the database, a registry and postgresql.org.
//
// Optional, asserted on Apps like Databases.
type DatabaseReleases interface {
	DatabaseRelease(ctx context.Context, ownerID, name string) (app.DatabaseRelease, error)
}

func (s *Server) databaseReleases() DatabaseReleases {
	d, _ := s.apps.(DatabaseReleases)
	return d
}

// DatabaseUpdates moves a database to the newer image of its version that
// DatabaseRelease offers. Optional, asserted on Apps like DatabaseReleases.
type DatabaseUpdates interface {
	UpdateDatabase(ctx context.Context, ownerID, name, actor string) (app.DatabaseUpdate, error)
}

func (s *Server) databaseUpdates() DatabaseUpdates {
	d, _ := s.apps.(DatabaseUpdates)
	return d
}

// databaseUpdate is the Update button. What it deploys is worked out again
// by the service rather than read from the form, so a page left open while
// the registry moved on deploys what is newest now.
func (s *Server) databaseUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	u, err := s.databaseUpdates().UpdateDatabase(ctx, owner.ID, name, s.actorEmail(r))
	switch {
	case err != nil:
		s.log.Error("database update", slog.String("app", name), slog.String("error", err.Error()))
		s.flashErr(w, r, databaseSentence(err))
	case u.Backup:
		auditDetail(ctx, "to", u.Label)
		auditDetail(ctx, "image", u.Image)
		s.flashOK(w, r, "Backing up "+name+" first. The update to "+u.Label+
			" deploys when the backup finishes, in a minute or two.")
	default:
		auditDetail(ctx, "to", u.Label)
		auditDetail(ctx, "image", u.Image)
		s.flashOK(w, r, "Updating "+name+" to "+u.Label+".")
	}
	http.Redirect(w, r, "/apps/"+name, http.StatusSeeOther)
}

// DatabaseReleaseData is the version line, or the warning in its place.
type DatabaseReleaseData struct {
	Name, Image string
	R           app.DatabaseRelease
}

// Tag is the part of the image after the colon, as a person wrote it:
// "16-alpine".
func (d DatabaseReleaseData) Tag() string {
	i := strings.LastIndex(d.Image, ":")
	if i < 0 || strings.Contains(d.Image[i:], "/") {
		return "latest"
	}
	return d.Image[i+1:]
}

func (s *Server) databaseRelease(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	a, err := s.apps.Get(ctx, owner.ID, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rel, err := s.databaseReleases().DatabaseRelease(ctx, owner.ID, name)
	if err != nil {
		// Nothing to say is said with nothing: the slot is left empty.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	d := DatabaseReleaseData{Name: a.Name, Image: a.Image, R: rel}
	if err := databaseReleaseBlock(d).Render(ctx, w); err != nil {
		s.log.Error("render database release", slog.String("error", err.Error()))
	}
}

func securityFixes(n int) string {
	if n == 1 {
		return "1 security fix"
	}
	return strconv.Itoa(n) + " security fixes"
}

// isSerious is a fix worth the red: high or critical.
func isSerious(severity string) bool {
	return severity == "critical" || severity == "high"
}

func severityStatus(severity string) string {
	switch severity {
	case "critical", "high":
		return "status-err"
	case "medium":
		return "status-warn"
	}
	return "status-neutral"
}

func outOfSupport(eol time.Time) string {
	if eol.IsZero() {
		return "It is out of support."
	}
	return "It ran out of support on " + eol.Format("2 January 2006") + "."
}

// quietRelease is what follows the version when there is nothing to do,
// or nothing but the update below it.
func quietRelease(r app.DatabaseRelease) string {
	switch {
	case r.Update != nil:
		return ""
	case r.Known && !r.Report.Behind():
		return " · the newest " + r.Report.Running.Major + " release"
	case r.Newest != "" && r.Newest == r.Version:
		return " · the newest " + r.Series + " release"
	case r.Known:
		return " · " + r.Report.Latest.String() + " is out, with no security fixes this lacks"
	}
	return ""
}

// updateHeadline is what the update row says first: "PostgreSQL 16.16 is
// available".
func updateHeadline(u *app.DatabaseUpdate) string {
	label := u.Label
	if label != "" {
		label = strings.ToUpper(label[:1]) + label[1:]
	}
	return label + " is available"
}

// updateEffect is what pressing Update does, said before it is pressed.
func updateEffect(d DatabaseReleaseData) string {
	u := d.R.Update
	var b strings.Builder
	if u.Backup {
		b.WriteString("A backup is taken first, then ")
	} else {
		b.WriteString("No backup is taken first, as this database cannot be backed up here; ")
	}
	if u.Image == d.Image {
		b.WriteString("the newer " + d.Image + " image deploys")
	} else {
		b.WriteString("the image changes from " + d.Image + " to " + u.Image)
	}
	b.WriteString(" and the database restarts, unavailable for a few seconds. Its storage is kept.")
	return b.String()
}

// updateConfirm is the confirmation dialog's text.
func updateConfirm(d DatabaseReleaseData) string {
	return updateEffect(d) + " If the new version does not become ready, the deploy fails " +
		"and the current version is put back."
}

// majorMigration is how to move to a new major version, which is never
// done in place: PostgreSQL will not start on a data directory an older
// major version wrote, and the other engines upgrade one way only.
func majorMigration(r app.DatabaseRelease) string {
	next := r.Engine + " " + r.NextMajor
	if r.Engine == "PostgreSQL" {
		// A backup restores only into the database it was taken from, so
		// the copy is pg_dump's and pg_restore's rather than the Backups tab's.
		return "create a new " + next + " service beside this one, copy the data across with " +
			"pg_dump and pg_restore, and point the apps at it."
	}
	return "create a new " + next + " service beside this one, copy the data across, and point the apps at it."
}

func fixesThem(n int) string {
	if n == 1 {
		return " fixes it."
	}
	return " fixes them."
}
