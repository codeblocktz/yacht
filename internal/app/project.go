package app

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// ErrProjectNotFound means the team has no project with that slug.
var ErrProjectNotFound = errors.New("app: no such project")

// DefaultProjectSlug is the project apps land in when nobody has chosen one.
//
// A team that never thinks about projects should never have to: there is
// always one, it is created on demand, and every app goes in it.
const DefaultProjectSlug = "default"

// DefaultProjectName is what the default project is called on screen.
const DefaultProjectName = "Default"

// Project groups the apps drawn on one canvas.
type Project struct {
	ID      uuid.UUID
	OwnerID string
	Slug    string
	Name    string

	// Apps is how many apps it holds. Populated by List, because a list of
	// projects that does not say how big each one is gives no reason to pick.
	Apps int64
}

var slugRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Slugify turns a display name into an addressable slug.
func Slugify(name string) string {
	var b strings.Builder
	lastDash := true // leading dashes are dropped, not collapsed into one
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	return s
}

// CreateProject makes a project for a team.
func (s *Service) CreateProject(ctx context.Context, ownerID, name string) (Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Project{}, errors.New("a project needs a name")
	}
	if len(name) > 100 {
		return Project{}, errors.New("project name must be at most 100 characters")
	}

	slug := Slugify(name)
	if !slugRE.MatchString(slug) {
		// Reached when the name is entirely punctuation or non-Latin: there is
		// nothing to build an address out of, and a generated one would be a
		// URL with no relationship to what the person typed.
		return Project{}, errors.New(
			"project name needs at least one letter or number to build an address from")
	}

	row, err := s.q.CreateProject(ctx, dbgen.CreateProjectParams{
		OwnerID: ownerID, Slug: slug, Name: name,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return Project{}, fmt.Errorf("a project called %q already exists", name)
		}
		return Project{}, fmt.Errorf("app: create project: %w", err)
	}
	return toProject(row), nil
}

// Projects lists a team's projects.
//
// The default project is created here when the team has none, so the page that
// lists projects is never empty for a team that has apps. Doing it on read is
// what lets a team that predates projects open the page and see their work,
// rather than an empty list and no way to explain it.
func (s *Service) Projects(ctx context.Context, ownerID string) ([]Project, error) {
	rows, err := s.q.ListProjects(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("app: list projects: %w", err)
	}
	if len(rows) == 0 {
		if _, err := s.DefaultProject(ctx, ownerID); err != nil {
			return nil, err
		}
		rows, err = s.q.ListProjects(ctx, ownerID)
		if err != nil {
			return nil, fmt.Errorf("app: list projects: %w", err)
		}
	}

	out := make([]Project, 0, len(rows))
	for _, r := range rows {
		out = append(out, Project{
			ID: r.ID, OwnerID: r.OwnerID, Slug: r.Slug, Name: r.Name, Apps: r.AppCount,
		})
	}
	return out, nil
}

// DefaultProject returns the team's default project, creating it if needed and
// adopting any app that has no project.
//
// Adoption is what makes the column nullable safe: an app created before
// projects existed, or one whose project was deleted, is not lost — it is
// simply waiting for the next read to place it.
func (s *Service) DefaultProject(ctx context.Context, ownerID string) (Project, error) {
	row, err := s.q.GetProjectBySlug(ctx, dbgen.GetProjectBySlugParams{
		OwnerID: ownerID, Slug: DefaultProjectSlug,
	})
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		row, err = s.q.CreateProject(ctx, dbgen.CreateProjectParams{
			OwnerID: ownerID, Slug: DefaultProjectSlug, Name: DefaultProjectName,
		})
		if err != nil {
			// Two requests can reach this at once — the first page load of a
			// team with a couple of tabs open. The loser of that race reads the
			// row the winner wrote rather than failing.
			if !isUniqueViolation(err) {
				return Project{}, fmt.Errorf("app: create default project: %w", err)
			}
			row, err = s.q.GetProjectBySlug(ctx, dbgen.GetProjectBySlugParams{
				OwnerID: ownerID, Slug: DefaultProjectSlug,
			})
			if err != nil {
				return Project{}, fmt.Errorf("app: read default project: %w", err)
			}
		}
	default:
		return Project{}, fmt.Errorf("app: get default project: %w", err)
	}

	if _, err := s.q.MoveAppsWithoutProject(ctx, dbgen.MoveAppsWithoutProjectParams{
		OwnerID: ownerID, ProjectID: pgUUID(row.ID),
	}); err != nil {
		return Project{}, fmt.Errorf("app: adopt unassigned apps: %w", err)
	}
	return toProject(row), nil
}

// Project returns one project by slug.
func (s *Service) Project(ctx context.Context, ownerID, slug string) (Project, error) {
	if slug == "" || slug == DefaultProjectSlug {
		return s.DefaultProject(ctx, ownerID)
	}
	row, err := s.q.GetProjectBySlug(ctx, dbgen.GetProjectBySlugParams{
		OwnerID: ownerID, Slug: slug,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Project{}, ErrProjectNotFound
		}
		return Project{}, fmt.Errorf("app: get project: %w", err)
	}
	return toProject(row), nil
}

// ProjectByID returns one project by id, for a caller holding an app.
func (s *Service) ProjectByID(ctx context.Context, ownerID string, id uuid.UUID) (Project, error) {
	row, err := s.q.GetProjectByID(ctx, dbgen.GetProjectByIDParams{OwnerID: ownerID, ID: id})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Project{}, ErrProjectNotFound
		}
		return Project{}, fmt.Errorf("app: get project: %w", err)
	}
	return toProject(row), nil
}

// ListInProject returns a project's apps, with cluster status, as List does.
func (s *Service) ListInProject(ctx context.Context, ownerID string, projectID uuid.UUID) ([]App, error) {
	rows, err := s.q.ListAppsInProject(ctx, dbgen.ListAppsInProjectParams{
		OwnerID: ownerID, ProjectID: pgUUID(projectID),
	})
	if err != nil {
		return nil, fmt.Errorf("app: list apps in project: %w", err)
	}

	out := make([]App, 0, len(rows))
	for _, row := range rows {
		a := toApp(row)
		s.attachStatus(ctx, &a)
		s.attachHost(ctx, &a)
		out = append(out, a)
	}
	return out, nil
}

// SetPosition records where somebody dragged an app's card to.
//
// Positions are the team's, not the person's: a canvas two people arrange
// differently is two pictures of one system, and neither can be pointed at in
// a conversation.
func (s *Service) SetPosition(ctx context.Context, ownerID, name string, x, y int32) error {
	n, err := s.q.SetAppPosition(ctx, dbgen.SetAppPositionParams{
		OwnerID: ownerID, Name: name, CanvasX: &x, CanvasY: &y,
	})
	if err != nil {
		return fmt.Errorf("app: set position: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearPositions forgets a project's arrangement so it lays out again.
func (s *Service) ClearPositions(ctx context.Context, ownerID string, projectID uuid.UUID) error {
	if _, err := s.q.ClearProjectPositions(ctx, dbgen.ClearProjectPositionsParams{
		OwnerID: ownerID, ProjectID: pgUUID(projectID),
	}); err != nil {
		return fmt.Errorf("app: clear positions: %w", err)
	}
	return nil
}

// pgUUID wraps a uuid for a column sqlc generated as nullable.
//
// The nil uuid becomes SQL NULL rather than a row of zero bytes. It is not a
// real id, nothing references it, and writing it into a foreign key would fail
// at the database with an error naming a constraint rather than the missing
// project — so the caller that forgot to pass one learns nothing useful.
func pgUUID(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

func toProject(row dbgen.Project) Project {
	return Project{ID: row.ID, OwnerID: row.OwnerID, Slug: row.Slug, Name: row.Name}
}
