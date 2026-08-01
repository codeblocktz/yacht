package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/cluster"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

const probeToken = "K10probe::server:hunter2"

type fakeJoiner struct {
	dns        string
	configured bool
	set        []string
	err        error
}

func (f *fakeJoiner) Settings(context.Context) (cluster.Settings, error) {
	if !f.configured {
		return cluster.Settings{}, cluster.ErrNotConfigured
	}
	return cluster.Settings{
		ServerURL: "https://10.0.0.1:6443", TokenSet: true, UpdatedAt: "2026-08-01 12:00",
	}, nil
}

func (f *fakeJoiner) SetJoin(_ context.Context, serverURL, token string, _ uuid.UUID) error {
	if f.err != nil {
		return f.err
	}
	f.set = append(f.set, serverURL+" "+token)
	f.configured = true
	return nil
}

func (f *fakeJoiner) DNS(context.Context) (cluster.DNS, error) {
	return cluster.DNS{CNAMETarget: "edge.yacht.test", TXTPrefix: "extdns-"}, nil
}

func (f *fakeJoiner) SetDNS(_ context.Context, target, prefix string) error {
	f.dns = target + " " + prefix
	return nil
}

func (f *fakeJoiner) Command(_ context.Context, pool string) (string, error) {
	if !f.configured {
		return "", cluster.ErrNotConfigured
	}
	return cluster.BuildCommand("https://10.0.0.1:6443", probeToken, pool), nil
}

// joinServer builds a server whose viewer holds role in the team.
func joinServer(t *testing.T, j Joiner, role account.Role) http.Handler {
	t.Helper()
	const team = "join-team"
	s, err := New(Options{
		Orchestrator:    orchestrator.NewNoop(),
		Apps:            newFakeApps(sampleApp(team, "probe")),
		Identity:        identity.NewSingleOwner(identity.Owner{ID: team}),
		Accounts:        &roledAccounts{fakeAccounts: &fakeAccounts{}, team: team, role: role},
		Mailer:          &fakeMailer{},
		BaseURL:         "https://yacht.test",
		BootstrapTeamID: team,
		Joiner:          j,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.Handler()
}

func signedIn(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: "probe-session"})
	return req
}

func do(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A node is cluster-scoped while every role here is team-scoped: a node one
// team adds runs every other team's workloads and can read the secrets mounted
// into them. Admin is a team role, so admin is not enough.
func TestOnlyAnOwnerCanReachTheJoinCommand(t *testing.T) {
	for _, tc := range []struct {
		role account.Role
		want int
	}{
		{account.RoleOwner, http.StatusOK},
		{account.RoleAdmin, http.StatusForbidden},
		{account.RoleMember, http.StatusForbidden},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			h := joinServer(t, &fakeJoiner{configured: true}, tc.role)

			if code := do(h, signedIn(http.MethodGet, "/cluster/nodes/add")).Code; code != tc.want {
				t.Errorf("GET add page as %s = %d, want %d", tc.role, code, tc.want)
			}
			if code := do(h, signedIn(http.MethodPost, "/cluster/join")).Code; code == http.StatusForbidden != (tc.want == http.StatusForbidden) {
				t.Errorf("POST join as %s = %d, which disagrees with the page", tc.role, code)
			}
		})
	}
}

// The token is what the command exists to deliver, so it appears there and
// nowhere else. Counted rather than eyeballed: a summary that started echoing
// the stored token would still look fine on screen.
func TestTheTokenAppearsOnlyInTheCommand(t *testing.T) {
	h := joinServer(t, &fakeJoiner{configured: true}, account.RoleOwner)

	body := do(h, signedIn(http.MethodGet, "/cluster/nodes/add")).Body.String()

	if n := strings.Count(body, probeToken); n != 1 {
		t.Fatalf("the join token appears %d times on the page, want exactly once", n)
	}
	// And where it appears is the command, not some other field that happens
	// to hold it.
	if !strings.Contains(body, "K3S_TOKEN="+probeToken) {
		t.Error("the token is on the page somewhere other than the join command")
	}
	// The page has to warn what the command is, because it looks like a URL.
	if !strings.Contains(body, "join token") {
		t.Error("nothing on the page says the command carries a credential")
	}
}

// Nothing stored is a prompt, not a command with holes in it.
func TestWithNoSettingsThePageAsksForThem(t *testing.T) {
	h := joinServer(t, &fakeJoiner{}, account.RoleOwner)

	body := do(h, signedIn(http.MethodGet, "/cluster/nodes/add")).Body.String()

	if strings.Contains(body, "curl -sfL") {
		t.Error("a join command was rendered with no settings stored")
	}
	for _, want := range []string{"node-token", `name="server_url"`, `name="token"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not ask for the join settings: missing %q", want)
		}
	}
}

// Building the command needs no cluster, so an unreachable one must not take
// the command away — it only stops the confirmation.
func TestAnUnreachableClusterStillYieldsACommand(t *testing.T) {
	const team = "join-team"
	s, err := New(Options{
		Orchestrator:    newFailingOrchestrator(),
		Apps:            newFakeApps(sampleApp(team, "probe")),
		Identity:        identity.NewSingleOwner(identity.Owner{ID: team}),
		Accounts:        &roledAccounts{fakeAccounts: &fakeAccounts{}, team: team, role: account.RoleOwner},
		Mailer:          &fakeMailer{},
		BaseURL:         "https://yacht.test",
		BootstrapTeamID: team,
		Joiner:          &fakeJoiner{configured: true},
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := do(s.Handler(), signedIn(http.MethodGet, "/cluster/nodes/add")).Body.String()

	if !strings.Contains(body, "K3S_TOKEN="+probeToken) {
		t.Error("an unreachable cluster took the join command away")
	}
	// And it says which of the two things is wrong. "No node yet" and "the
	// cluster is not answering" look identical and mean different things.
	if !strings.Contains(body, "not answering") {
		t.Error("the page does not distinguish an unreachable cluster from no node")
	}
}

// The pool rides in the command, and a pool that could end the command is
// refused before it gets there.
func TestThePoolRidesInTheCommandAndIsChecked(t *testing.T) {
	h := joinServer(t, &fakeJoiner{configured: true}, account.RoleOwner)

	body := do(h, signedIn(http.MethodGet, "/cluster/nodes/add?pool=gpu")).Body.String()
	if !strings.Contains(body, "--node-label yacht/pool=gpu") {
		t.Error("the chosen pool is not in the command")
	}

	body = do(h, signedIn(http.MethodGet, "/cluster/nodes/add?pool=web%3Bcurl+evil.sh")).Body.String()
	if strings.Contains(body, "curl evil.sh") {
		t.Fatal("a pool name that ends the command reached the page")
	}
}
