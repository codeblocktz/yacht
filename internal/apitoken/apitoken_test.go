package apitoken

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/store"
)

// fixture is a team with an owner, an admin and a member, against the test
// database. Teams begin tok- and addresses end @tok.test, so this package's
// purge touches nothing another package's tests made.
type fixture struct {
	pool    *pgxpool.Pool
	tokens  *Service
	acct    *account.Service
	team    string
	owner   uuid.UUID
	admin   uuid.UUID
	member  uuid.UUID
	project uuid.UUID
}

func newFixture(t *testing.T, team string) *fixture {
	t.Helper()
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run api token tests")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, dsn, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	purge := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM cli_logins WHERE owner_id LIKE 'tok-%' OR owner_id IS NULL`)
		_, _ = pool.Exec(ctx, `DELETE FROM teams WHERE id LIKE 'tok-%'`)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE lower(email) LIKE '%@tok.test'`)
	}
	purge()
	t.Cleanup(purge)

	acct := account.NewService(pool, log)
	f := &fixture{pool: pool, tokens: New(pool, log, ""), acct: acct, team: team}
	person := func(email string) uuid.UUID {
		u, err := acct.EnsureUser(ctx, email, "")
		if err != nil {
			t.Fatalf("EnsureUser: %v", err)
		}
		return u.ID
	}
	f.owner = person("owner-" + team + "@tok.test")
	f.admin = person("admin-" + team + "@tok.test")
	f.member = person("member-" + team + "@tok.test")
	if _, err := acct.CreateTeam(ctx, team, team, f.owner); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := acct.SetRole(ctx, f.owner, team, f.admin, account.RoleAdmin); err != nil {
		t.Fatalf("SetRole admin: %v", err)
	}
	if err := acct.SetRole(ctx, f.owner, team, f.member, account.RoleMember); err != nil {
		t.Fatalf("SetRole member: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO projects (owner_id, name, slug) VALUES ($1, 'Web', 'web') RETURNING id`,
		team).Scan(&f.project); err != nil {
		t.Fatalf("project: %v", err)
	}
	return f
}

func TestATokenIsShownOnceAndStoredOnlyAsItsHash(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "tok-hash")
	raw, tok, err := f.tokens.Create(ctx, CreateInput{OwnerID: f.team, Name: "laptop", Role: account.RoleMember, Actor: f.member})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(raw, DefaultPrefix) || len(raw) < 40 {
		t.Fatalf("token %q does not look like one", raw)
	}
	if tok.Hint != raw[len(raw)-4:] {
		t.Errorf("hint = %q, want the token's last four characters", tok.Hint)
	}
	// Nothing in the row is the token: not the hash column, not any other.
	var row []byte
	if err := f.pool.QueryRow(ctx, `SELECT row_to_json(t)::text::bytea FROM api_tokens t WHERE id = $1`, tok.ID).Scan(&row); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if bytes.Contains(row, []byte(raw)) || bytes.Contains(row, []byte(raw[len(DefaultPrefix):])) {
		t.Fatal("the stored row contains the token itself")
	}
	p, err := f.tokens.Resolve(ctx, raw)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.OwnerID != f.team || p.Role != account.RoleMember || p.TokenID != tok.ID {
		t.Errorf("resolved %+v", p)
	}
	for _, bad := range []string{"", raw + "x", DefaultPrefix + "nope", strings.ToUpper(raw)} {
		if _, err := f.tokens.Resolve(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("Resolve(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestATokenCanBeNoStrongerThanItsMaker(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "tok-cap")
	if _, _, err := f.tokens.Create(ctx, CreateInput{OwnerID: f.team, Name: "x", Role: account.RoleAdmin, Actor: f.member}); !errors.Is(err, ErrRole) {
		t.Errorf("member making an admin token = %v, want ErrRole", err)
	}
	if _, _, err := f.tokens.Create(ctx, CreateInput{OwnerID: f.team, Name: "x", Role: account.RoleOwner, Actor: f.owner}); !errors.Is(err, ErrRole) {
		t.Errorf("an owner token = %v, want ErrRole: nothing the API does needs one", err)
	}
	stranger, _ := f.acct.EnsureUser(ctx, "stranger@tok.test", "")
	if _, _, err := f.tokens.Create(ctx, CreateInput{OwnerID: f.team, Name: "x", Role: account.RoleMember, Actor: stranger.ID}); !errors.Is(err, ErrForbidden) {
		t.Errorf("somebody outside the team = %v, want ErrForbidden", err)
	}
	if _, _, err := f.tokens.Create(ctx, CreateInput{OwnerID: f.team, Name: " ", Role: account.RoleMember, Actor: f.member}); !errors.Is(err, ErrName) {
		t.Errorf("blank name = %v, want ErrName", err)
	}
	// A project of another team is not this team's to make a token for.
	other := uuid.New()
	if _, _, err := f.tokens.Create(ctx, CreateInput{OwnerID: f.team, ProjectID: &other, Name: "x", Role: account.RoleMember, Actor: f.admin}); !errors.Is(err, ErrNotFound) {
		t.Errorf("project not the team's = %v, want ErrNotFound", err)
	}
}

// The reason created_by is joined on every lookup.
func TestATokenActsWithItsMakersCurrentAuthority(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "tok-maker")
	raw, _, err := f.tokens.Create(ctx, CreateInput{OwnerID: f.team, Name: "ci", Role: account.RoleAdmin, Actor: f.admin})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.acct.SetRole(ctx, f.owner, f.team, f.admin, account.RoleMember); err != nil {
		t.Fatalf("demote: %v", err)
	}
	p, err := f.tokens.Resolve(ctx, raw)
	if err != nil || p.Role != account.RoleMember {
		t.Fatalf("after its maker was demoted the token is %v/%v, want a member's", p.Role, err)
	}
	if err := f.acct.RemoveMember(ctx, f.owner, f.team, f.admin); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := f.tokens.Resolve(ctx, raw); !errors.Is(err, ErrInvalid) {
		t.Fatalf("after its maker left the team the token resolves (%v), want ErrInvalid", err)
	}
}

func TestRevokingIsTheMakersOrAnAdmins(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "tok-revoke")
	raw, tok, _ := f.tokens.Create(ctx, CreateInput{OwnerID: f.team, Name: "admin's", Role: account.RoleMember, Actor: f.admin})
	if _, err := f.tokens.Revoke(ctx, f.team, f.member, tok.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("a member revoking somebody else's = %v, want ErrForbidden", err)
	}
	mine, mineTok, _ := f.tokens.Create(ctx, CreateInput{OwnerID: f.team, Name: "member's", Role: account.RoleMember, Actor: f.member})
	if _, err := f.tokens.Revoke(ctx, f.team, f.member, mineTok.ID); err != nil {
		t.Errorf("a member revoking their own = %v", err)
	}
	if _, err := f.tokens.Resolve(ctx, mine); !errors.Is(err, ErrInvalid) {
		t.Error("a revoked token still resolves")
	}
	if _, err := f.tokens.Revoke(ctx, "tok-elsewhere", f.owner, tok.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking through another team = %v, want ErrNotFound", err)
	}
	if _, err := f.tokens.Revoke(ctx, f.team, f.owner, tok.ID); err != nil {
		t.Errorf("an owner revoking = %v", err)
	}
	if _, err := f.tokens.Resolve(ctx, raw); !errors.Is(err, ErrInvalid) {
		t.Error("a revoked token still resolves")
	}
	list, _ := f.tokens.List(ctx, f.team)
	if len(list) != 0 {
		t.Errorf("revoked tokens are still listed: %d", len(list))
	}
}

func TestAProjectTokenNamesItsProject(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "tok-project")
	raw, _, err := f.tokens.Create(ctx, CreateInput{OwnerID: f.team, ProjectID: &f.project, Name: "deploy", Role: account.RoleMember, Actor: f.admin})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	p, err := f.tokens.Resolve(ctx, raw)
	if err != nil || p.ProjectID == nil || *p.ProjectID != f.project || p.ProjectSlug != "web" {
		t.Fatalf("resolved %+v, %v", p, err)
	}
}

func TestBrowserLoginMintsOneTokenForTheApprover(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "tok-login")
	device, code, _, err := f.tokens.StartLogin(ctx, "laptop.local")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if _, _, err := f.tokens.PollLogin(ctx, device); !errors.Is(err, ErrLoginPending) {
		t.Fatalf("poll before approval = %v, want pending", err)
	}
	if err := f.tokens.ApproveLogin(ctx, code, f.member, f.team, account.RoleAdmin); !errors.Is(err, ErrRole) {
		t.Fatalf("a member approving as admin = %v, want ErrRole", err)
	}
	if err := f.tokens.ApproveLogin(ctx, strings.ToLower(strings.ReplaceAll(code, "-", "")), f.member, f.team, account.RoleMember); err != nil {
		t.Fatalf("ApproveLogin with the code as typed: %v", err)
	}
	// Two polls at once: one token, not two.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var got []string
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if raw, _, err := f.tokens.PollLogin(ctx, device); err == nil {
				mu.Lock()
				got = append(got, raw)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(got) != 1 {
		t.Fatalf("%d polls were given a token, want exactly one", len(got))
	}
	p, err := f.tokens.Resolve(ctx, got[0])
	if err != nil || p.MakerID != f.member || p.Role != account.RoleMember {
		t.Fatalf("the CLI's token resolves to %+v, %v", p, err)
	}
	if _, _, err := f.tokens.PollLogin(ctx, device); !errors.Is(err, ErrLoginExpired) {
		t.Errorf("a spent login polled again = %v, want ErrLoginExpired", err)
	}

	device2, code2, _, _ := f.tokens.StartLogin(ctx, "")
	if err := f.tokens.DenyLogin(ctx, code2); err != nil {
		t.Fatalf("DenyLogin: %v", err)
	}
	if _, _, err := f.tokens.PollLogin(ctx, device2); !errors.Is(err, ErrLoginDenied) {
		t.Errorf("a refused login = %v, want ErrLoginDenied", err)
	}
	if _, _, err := f.tokens.PollLogin(ctx, "not-a-device"); !errors.Is(err, ErrLoginExpired) {
		t.Errorf("an unknown device = %v, want ErrLoginExpired", err)
	}
}
