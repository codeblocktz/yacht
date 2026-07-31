package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// ErrLastOwner is returned when a change would leave a team with no owner.
var ErrLastOwner = errors.New("account: a team must keep at least one owner")

// ErrNotAMember is returned when a person holds no role in a team.
var ErrNotAMember = errors.New("account: not a member of this team")

// ErrForbidden is returned when the actor's role does not carry the authority
// the change needs.
var ErrForbidden = errors.New("account: not permitted")

// ErrTeamTaken is returned when the team id already belongs to someone else.
var ErrTeamTaken = errors.New("account: a team with that id already exists")

// Role is what a person may do in a team.
//
// The split is at the line where an action stops being undoable by
// redeploying: a member deploys, scales and redeploys; an admin can also
// delete apps, manage domains and storage, and invite people; an owner can
// also manage admins and the team itself.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

var rank = map[Role]int{RoleMember: 1, RoleAdmin: 2, RoleOwner: 3}

// AtLeast reports whether r carries at least the authority of other.
func (r Role) AtLeast(other Role) bool { return rank[r] >= rank[other] && rank[r] > 0 }

// CanAdminister covers deleting apps, managing domains and storage, and
// inviting people.
func (r Role) CanAdminister() bool { return r.AtLeast(RoleAdmin) }

// CanOwn covers managing admins, transferring ownership and deleting the team.
func (r Role) CanOwn() bool { return r == RoleOwner }

// Valid reports whether r is a role the system knows.
func (r Role) Valid() bool { _, ok := rank[r]; return ok }

// User is a person.
type User struct {
	ID          uuid.UUID
	Email       string
	DisplayName string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Team is a principal: the thing owner_id points at everywhere else.
type Team struct {
	ID          string
	DisplayName string
	Email       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Membership is a person's place in a team, carrying the team's name so a
// switcher can be rendered without a query per row.
type Membership struct {
	UserID    uuid.UUID
	TeamID    string
	TeamName  string
	Role      Role
	CreatedAt time.Time
}

// Service owns people, teams and the roles between them.
type Service struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
	log  *slog.Logger
}

// NewService wires the store into the account service.
func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, q: dbgen.New(pool), log: log}
}

// EnsureUser returns the user for an address, creating them if new.
//
// The address is lowercased on the way in, because a person who signs up as
// Alice@ and is later invited as alice@ is one person. An empty display name
// leaves the stored one alone: sign-in knows the address and not the name, and
// must not blank out what the person set.
func (s *Service) EnsureUser(ctx context.Context, email, displayName string) (User, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return User{}, errors.New("account: email is required")
	}
	row, err := s.q.UpsertUser(ctx, dbgen.UpsertUserParams{
		Email: email, DisplayName: strings.TrimSpace(displayName),
	})
	if err != nil {
		return User{}, fmt.Errorf("account: ensure user: %w", err)
	}
	return toUser(row), nil
}

// CreateTeam creates a team and makes the creator its owner.
//
// Both writes happen in one transaction: a team whose creator is not a member
// is a team nobody can administer, and it would take a database edit to fix.
//
// An id already held by another team is refused rather than adopted. The
// underlying upsert exists so an operator can re-run creation idempotently,
// but without this check picking an existing id would hand the caller
// ownership of that team's apps.
func (s *Service) CreateTeam(
	ctx context.Context, id, name string, ownerUserID uuid.UUID,
) (Team, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Team{}, errors.New("account: team id is required")
	}
	if ownerUserID == uuid.Nil {
		return Team{}, errors.New("account: a team needs an owner")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Team{}, fmt.Errorf("account: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	q := s.q.WithTx(tx)

	owners, err := q.CountOwnersOfTeam(ctx, id)
	if err != nil {
		return Team{}, fmt.Errorf("account: count owners: %w", err)
	}
	if owners > 0 {
		role, err := roleIn(ctx, q, ownerUserID, id)
		if err != nil && !errors.Is(err, ErrNotAMember) {
			return Team{}, err
		}
		if role != RoleOwner {
			return Team{}, ErrTeamTaken
		}
	}

	row, err := q.CreateTeam(ctx, dbgen.CreateTeamParams{ID: id, DisplayName: name})
	if err != nil {
		return Team{}, fmt.Errorf("account: create team: %w", err)
	}
	if _, err := q.UpsertMembership(ctx, dbgen.UpsertMembershipParams{
		UserID: ownerUserID, OwnerID: id, Role: string(RoleOwner),
	}); err != nil {
		return Team{}, fmt.Errorf("account: create owner membership: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Team{}, fmt.Errorf("account: commit: %w", err)
	}

	s.log.Info("team created",
		slog.String("team", id), slog.String("owner", ownerUserID.String()))
	return toTeam(row), nil
}

// TeamsFor returns every team a person belongs to, with the role they hold.
func (s *Service) TeamsFor(ctx context.Context, userID uuid.UUID) ([]Membership, error) {
	rows, err := s.q.ListMembershipsForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("account: list teams: %w", err)
	}
	out := make([]Membership, 0, len(rows))
	for _, row := range rows {
		out = append(out, Membership{
			UserID:    row.UserID,
			TeamID:    row.OwnerID,
			TeamName:  row.TeamName,
			Role:      Role(row.Role),
			CreatedAt: row.CreatedAt,
		})
	}
	return out, nil
}

// RoleIn returns the role a person holds in a team.
//
// Absence is ErrNotAMember rather than an empty role, so a caller cannot
// mistake "no membership" for "least privilege" and let a stranger in as a
// member.
func (s *Service) RoleIn(ctx context.Context, userID uuid.UUID, teamID string) (Role, error) {
	return roleIn(ctx, s.q, userID, teamID)
}

func roleIn(
	ctx context.Context, q *dbgen.Queries, userID uuid.UUID, teamID string,
) (Role, error) {
	row, err := q.GetMembership(ctx, dbgen.GetMembershipParams{
		UserID: userID, OwnerID: teamID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotAMember
		}
		return "", fmt.Errorf("account: role in team: %w", err)
	}
	return Role(row.Role), nil
}

// SetRole gives target a role in a team, adding them if they are not a member.
func (s *Service) SetRole(
	ctx context.Context, actor uuid.UUID, teamID string, target uuid.UUID, role Role,
) error {
	if !role.Valid() {
		return fmt.Errorf("account: unknown role %q", role)
	}
	return s.inTeam(ctx, teamID, func(q *dbgen.Queries) error {
		current, err := s.authorise(ctx, q, actor, teamID, target, role)
		if err != nil {
			return err
		}
		if current == RoleOwner && role != RoleOwner {
			if err := lastOwner(ctx, q, teamID); err != nil {
				return err
			}
		}
		if _, err := q.UpsertMembership(ctx, dbgen.UpsertMembershipParams{
			UserID: target, OwnerID: teamID, Role: string(role),
		}); err != nil {
			return fmt.Errorf("account: set role: %w", err)
		}
		return nil
	})
}

// RemoveMember takes a person out of a team.
func (s *Service) RemoveMember(
	ctx context.Context, actor uuid.UUID, teamID string, target uuid.UUID,
) error {
	return s.inTeam(ctx, teamID, func(q *dbgen.Queries) error {
		current, err := s.authorise(ctx, q, actor, teamID, target, "")
		if err != nil {
			return err
		}
		if current == "" {
			return ErrNotAMember
		}
		if current == RoleOwner {
			if err := lastOwner(ctx, q, teamID); err != nil {
				return err
			}
		}
		if err := q.DeleteMembership(ctx, dbgen.DeleteMembershipParams{
			UserID: target, OwnerID: teamID,
		}); err != nil {
			return fmt.Errorf("account: remove member: %w", err)
		}
		return nil
	})
}

// authorise reports the target's current role, having checked that the actor
// may make the change from it to next.
//
// Touching anyone who can administer — appointing an admin, demoting one,
// removing an owner — is ownership. An admin allowed to appoint another admin
// could appoint an owner next, which is every permission the team has.
func (s *Service) authorise(
	ctx context.Context, q *dbgen.Queries,
	actor uuid.UUID, teamID string, target uuid.UUID, next Role,
) (Role, error) {
	actorRole, err := roleIn(ctx, q, actor, teamID)
	if err != nil {
		return "", err
	}
	if !actorRole.CanAdminister() {
		return "", ErrForbidden
	}

	current, err := roleIn(ctx, q, target, teamID)
	if err != nil && !errors.Is(err, ErrNotAMember) {
		return "", err
	}
	if !actorRole.CanOwn() && (current.CanAdminister() || next.CanAdminister()) {
		return "", ErrForbidden
	}
	return current, nil
}

// lastOwner refuses a change that would leave the team with no owner.
//
// Counted inside the caller's transaction, which holds the team row: the count
// is read and then acted on, and without that lock two concurrent demotions
// would both see two owners and the team would end with none. A team with no
// owner cannot be administered by anyone, ever again.
func lastOwner(ctx context.Context, q *dbgen.Queries, teamID string) error {
	n, err := q.CountOwnersOfTeam(ctx, teamID)
	if err != nil {
		return fmt.Errorf("account: count owners: %w", err)
	}
	if n <= 1 {
		return ErrLastOwner
	}
	return nil
}

// inTeam runs fn in a transaction holding the team row.
func (s *Service) inTeam(
	ctx context.Context, teamID string, fn func(q *dbgen.Queries) error,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("account: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	q := s.q.WithTx(tx)
	if _, err := q.LockTeam(ctx, teamID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Nobody is a member of a team that does not exist, and saying so
			// keeps a probe for team ids indistinguishable from a permission
			// failure.
			return ErrNotAMember
		}
		return fmt.Errorf("account: lock team: %w", err)
	}

	if err := fn(q); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("account: commit: %w", err)
	}
	return nil
}

func toUser(row dbgen.User) User {
	return User{
		ID:          row.ID,
		Email:       row.Email,
		DisplayName: row.DisplayName,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
}

func toTeam(row dbgen.Team) Team {
	return Team{
		ID:          row.ID,
		DisplayName: row.DisplayName,
		Email:       row.Email,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
}
