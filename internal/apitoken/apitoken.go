// Package apitoken owns the credentials programs hold instead of a session:
// a CLI on somebody's laptop, a CI job deploying on merge.
//
// Two kinds, the way Railway has them. A team token reaches everything the
// team has; a project token reaches one project's apps and nothing else. Both
// carry a role — member or admin, never owner — and both act with no more
// authority than the person who made them still holds, because every lookup
// joins that person's membership. Removing somebody from a team ends their
// tokens on the same request, and demoting an admin makes their admin tokens
// act as a member's.
//
// A token is shown once, when it is made, and only its SHA-256 is stored —
// the rule sessions, magic links and invitations already follow.
package apitoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// DefaultPrefix begins every token the engine issues. A prefix is what lets a
// secret scanner — GitHub's, a pre-commit hook — recognise one pasted into a
// repository; a wrapper sets its own product's.
const DefaultPrefix = "yacht_"

// tokenBytes matches account's: beyond brute force, and free.
const tokenBytes = 32

var (
	// ErrInvalid is every token that cannot be used: unknown, revoked, or
	// made by somebody no longer in the team. One error, so nothing that
	// surfaces it can tell a revoked token from a guess.
	ErrInvalid = errors.New("apitoken: token is not valid")

	// ErrName is a name that cannot be shown.
	ErrName = errors.New("apitoken: a token's name is 1 to 60 characters, on one line")

	// ErrRole is a role a token cannot hold, or one higher than its maker's.
	ErrRole = errors.New("apitoken: a token is a member's or an admin's, and no more than its maker's")

	// ErrForbidden is a change the actor's role does not allow.
	ErrForbidden = errors.New("apitoken: not permitted")

	// ErrNotFound is a token that is not the team's, or no longer exists.
	ErrNotFound = errors.New("apitoken: no such token")
)

// Token is one credential as a page lists it. The token itself is not here:
// it exists only in what Create returns.
type Token struct {
	ID      uuid.UUID
	OwnerID string

	// ProjectID is set on a project token; ProjectSlug and ProjectName name
	// the project for a list.
	ProjectID   *uuid.UUID
	ProjectSlug string
	ProjectName string

	Name string
	Role account.Role
	// Hint is the token's last four characters, so a list can say which is
	// which without holding anything usable.
	Hint   string
	Origin string

	CreatedBy      uuid.UUID
	CreatedByEmail string
	CreatedAt      time.Time
	LastUsedAt     *time.Time
	LastUsedIP     string
}

// Project reports whether this is a project token.
func (t Token) Project() bool { return t.ProjectID != nil }

// Principal is who a request presenting a token acts as.
type Principal struct {
	TokenID   uuid.UUID
	TokenName string

	// OwnerID is the team; TeamName and TeamEmail describe it, as a session
	// does, so the identity a handler sees is the same shape either way.
	OwnerID   string
	TeamName  string
	TeamEmail string

	// Role is what the token may do now: the lesser of its own role and its
	// maker's current one.
	Role account.Role

	// ProjectID, when set, is the one project the token reaches.
	ProjectID   *uuid.UUID
	ProjectSlug string

	MakerID    uuid.UUID
	MakerEmail string
}

// Service issues, lists, revokes and resolves tokens.
type Service struct {
	pool   *pgxpool.Pool
	q      *dbgen.Queries
	log    *slog.Logger
	prefix string
}

// New wires the store into the service. An empty prefix is DefaultPrefix.
func New(pool *pgxpool.Pool, log *slog.Logger, prefix string) *Service {
	if log == nil {
		log = slog.Default()
	}
	if prefix == "" {
		prefix = DefaultPrefix
	}
	return &Service{pool: pool, q: dbgen.New(pool), log: log, prefix: prefix}
}

// Prefix is what every token this service issues begins with.
func (s *Service) Prefix() string { return s.prefix }

// CreateInput is a token to make.
type CreateInput struct {
	OwnerID   string
	ProjectID *uuid.UUID
	Name      string
	Role      account.Role
	// Origin is "dashboard" or "cli".
	Origin string
	// Actor is the person making it, whose membership is checked here and
	// whose authority the token can never exceed.
	Actor uuid.UUID
}

// Create makes a token and returns it, once. The caller shows it and keeps
// nothing: what is stored is its hash.
func (s *Service) Create(ctx context.Context, in CreateInput) (string, Token, error) {
	return s.create(ctx, s.q, in)
}

func (s *Service) create(ctx context.Context, q *dbgen.Queries, in CreateInput) (string, Token, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || utf8.RuneCountInString(name) > 60 || strings.ContainsAny(name, "\r\n") {
		return "", Token{}, ErrName
	}
	if in.Role != account.RoleMember && in.Role != account.RoleAdmin {
		return "", Token{}, ErrRole
	}
	if in.Origin == "" {
		in.Origin = "dashboard"
	}
	// The maker's role is read here, from their membership, rather than taken
	// from the caller. A token is made by a person in a team, and one they
	// could make above their own role would be a way to promote themselves.
	m, err := q.GetMembership(ctx, dbgen.GetMembershipParams{UserID: in.Actor, OwnerID: in.OwnerID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", Token{}, ErrForbidden
		}
		return "", Token{}, fmt.Errorf("apitoken: read maker's role: %w", err)
	}
	if !account.Role(m.Role).AtLeast(in.Role) {
		return "", Token{}, ErrRole
	}
	// A project id arrives from a request, so it is checked against the
	// team rather than trusted: a project token for another team's project
	// would be no use to anybody, but it is still not this team's to make.
	if in.ProjectID != nil {
		ok, err := q.ProjectBelongsToTeam(ctx, dbgen.ProjectBelongsToTeamParams{
			ID: *in.ProjectID, OwnerID: in.OwnerID,
		})
		if err != nil || !ok {
			return "", Token{}, ErrNotFound
		}
	}

	raw, hash, err := s.mint()
	if err != nil {
		return "", Token{}, err
	}
	row, err := q.CreateAPIToken(ctx, dbgen.CreateAPITokenParams{
		OwnerID: in.OwnerID, ProjectID: optionalUUID(in.ProjectID), Name: name,
		Role: string(in.Role), TokenHash: hash, Hint: raw[len(raw)-4:],
		Origin: in.Origin, CreatedBy: in.Actor,
	})
	if err != nil {
		return "", Token{}, fmt.Errorf("apitoken: create: %w", err)
	}
	s.log.Info("api token created",
		slog.String("team", in.OwnerID), slog.String("token", row.ID.String()),
		slog.String("role", row.Role), slog.Bool("project", in.ProjectID != nil))
	return raw, fromRow(row), nil
}

// mint returns a fresh token and the hash to store for it.
func (s *Service) mint() (string, []byte, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("apitoken: generate: %w", err)
	}
	raw := s.prefix + base64.RawURLEncoding.EncodeToString(b)
	return raw, hash(raw), nil
}

func hash(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// Resolve returns who a presented token acts as.
//
// Looked up by hash, so no raw value is ever compared; the comparison after
// the lookup is constant time all the same, so that this stays correct if the
// lookup is ever changed to fetch by something other than the hash.
func (s *Service) Resolve(ctx context.Context, raw string) (Principal, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, s.prefix) || len(raw) > 200 {
		return Principal{}, ErrInvalid
	}
	h := hash(raw)
	row, err := s.q.ResolveAPIToken(ctx, h)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Principal{}, ErrInvalid
		}
		return Principal{}, fmt.Errorf("apitoken: resolve: %w", err)
	}
	if subtle.ConstantTimeCompare(row.TokenHash, h) != 1 {
		return Principal{}, ErrInvalid
	}
	role := account.Role(row.Role)
	if maker := account.Role(row.MakerRole); !maker.AtLeast(role) {
		role = maker
	}
	p := Principal{
		TokenID: row.ID, TokenName: row.Name,
		OwnerID: row.OwnerID, TeamName: row.TeamName, TeamEmail: row.TeamEmail,
		Role: role, MakerID: row.CreatedBy, MakerEmail: row.MakerEmail,
	}
	if row.ProjectID.Valid {
		id := uuid.UUID(row.ProjectID.Bytes)
		p.ProjectID = &id
		if row.ProjectSlug != nil {
			p.ProjectSlug = *row.ProjectSlug
		}
	}
	return p, nil
}

// Touch records that a token was used, at most once a minute.
func (s *Service) Touch(ctx context.Context, id uuid.UUID, ip string) {
	if err := s.q.TouchAPIToken(ctx, dbgen.TouchAPITokenParams{ID: id, Ip: ip}); err != nil {
		s.log.Warn("record api token use", slog.String("error", err.Error()))
	}
}

// List is a team's live tokens, newest first: team and project tokens both.
func (s *Service) List(ctx context.Context, ownerID string) ([]Token, error) {
	rows, err := s.q.ListAPITokens(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("apitoken: list: %w", err)
	}
	out := make([]Token, 0, len(rows))
	for _, r := range rows {
		t := fromRow(dbgen.ApiToken{
			ID: r.ID, OwnerID: r.OwnerID, ProjectID: r.ProjectID, Name: r.Name, Role: r.Role,
			Hint: r.Hint, Origin: r.Origin, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt,
			LastUsedAt: r.LastUsedAt, LastUsedIp: r.LastUsedIp,
		})
		t.CreatedByEmail = r.MakerEmail
		if r.ProjectSlug != nil {
			t.ProjectSlug = *r.ProjectSlug
		}
		if r.ProjectName != nil {
			t.ProjectName = *r.ProjectName
		}
		out = append(out, t)
	}
	return out, nil
}

// Revoke ends a token. An admin may revoke any of the team's; anybody else
// only their own — a member who made a token for their laptop must be able
// to take it back without asking.
func (s *Service) Revoke(ctx context.Context, ownerID string, actor uuid.UUID, id uuid.UUID) (Token, error) {
	row, err := s.q.GetAPIToken(ctx, dbgen.GetAPITokenParams{OwnerID: ownerID, ID: id})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Token{}, ErrNotFound
		}
		return Token{}, fmt.Errorf("apitoken: read: %w", err)
	}
	if row.CreatedBy != actor {
		m, err := s.q.GetMembership(ctx, dbgen.GetMembershipParams{UserID: actor, OwnerID: ownerID})
		if err != nil || !account.Role(m.Role).CanAdminister() {
			return Token{}, ErrForbidden
		}
	}
	if _, err := s.q.RevokeAPIToken(ctx, dbgen.RevokeAPITokenParams{OwnerID: ownerID, ID: id}); err != nil {
		return Token{}, fmt.Errorf("apitoken: revoke: %w", err)
	}
	s.log.Info("api token revoked", slog.String("team", ownerID), slog.String("token", id.String()))
	return fromRow(row), nil
}

func fromRow(r dbgen.ApiToken) Token {
	t := Token{
		ID: r.ID, OwnerID: r.OwnerID, Name: r.Name, Role: account.Role(r.Role),
		Hint: r.Hint, Origin: r.Origin, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt,
		LastUsedIP: r.LastUsedIp,
	}
	if r.ProjectID.Valid {
		id := uuid.UUID(r.ProjectID.Bytes)
		t.ProjectID = &id
	}
	if r.LastUsedAt.Valid {
		at := r.LastUsedAt.Time
		t.LastUsedAt = &at
	}
	return t
}

func optionalUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}
