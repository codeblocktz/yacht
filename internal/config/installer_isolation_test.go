package config

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// A new install is written strict, and that is what the engine then runs on.
// The file is the only place the choice lives, so it is checked through Load
// as well as by its text: a value the engine would read as something else is
// not strict, whatever the file says.
func TestInstallerWritesANewInstallStrict(t *testing.T) {
	lib := sourceableInstaller(t)
	envFile := filepath.Join(t.TempDir(), "yacht.env")

	rendered := renderEnv(t, lib, envFile)
	if got := valueOf(rendered, "YACHT_NETWORK_ISOLATION"); got != "strict" {
		t.Fatalf("a new install has YACHT_NETWORK_ISOLATION=%q, want strict", got)
	}
	if !strings.Contains(rendered, "\nYACHT_EGRESS_ALLOW=\n") {
		t.Error("a new install has no YACHT_EGRESS_ALLOW line to name a LAN registry in")
	}
	if got := isolationLoaded(t, rendered); got != orchestrator.IsolationStrict {
		t.Errorf("the engine reads a new install's file as %q, want strict", got)
	}

	// And stays strict when the installer is run again over it.
	if err := os.WriteFile(envFile, []byte(rendered), 0o600); err != nil {
		t.Fatal(err)
	}
	if again := renderEnv(t, lib, envFile); again != rendered {
		t.Errorf("a re-run changed a new install's file\n--- first ---\n%s\n--- second ---\n%s", rendered, again)
	}
}

// An install made before the installer wrote the setting has run on team, the
// engine's default with the line absent. Re-running the installer over it
// writes that down rather than tightening the network under its apps, and
// whatever an operator set by hand — strict, off, the ranges let out — is
// kept, where before a re-run dropped both lines.
func TestInstallerKeepsAnExistingInstallsIsolation(t *testing.T) {
	lib := sourceableInstaller(t)
	envFile := filepath.Join(t.TempDir(), "yacht.env")

	existing := strings.Join([]string{
		"YACHT_DATABASE_URL=postgres://yacht:old@127.0.0.1:5432/yacht?sslmode=disable",
		"YACHT_AUTH_TOKEN=0123456789abcdef0123456789abcdef0123456789abcdef",
		"YACHT_SECRET_KEY=" + strings.Repeat("A", 43) + "=",
		"",
	}, "\n")

	// The engine itself, given that file as it was, runs on team.
	if got := isolationLoaded(t, existing); got != orchestrator.IsolationTeam {
		t.Fatalf("an existing file without the line loads as %q, want team", got)
	}

	if err := os.WriteFile(envFile, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	rendered := renderEnv(t, lib, envFile)
	if got := valueOf(rendered, "YACHT_NETWORK_ISOLATION"); got != "team" {
		t.Errorf("a re-run over an existing install wrote YACHT_NETWORK_ISOLATION=%q, want team", got)
	}
	if got := isolationLoaded(t, rendered); got != orchestrator.IsolationTeam {
		t.Errorf("after a re-run the engine reads %q, want team", got)
	}

	for _, set := range []struct{ mode, allow string }{
		{"strict", "10.20.0.5/32,192.168.1.0/24"},
		{"off", ""},
		{"team", ""},
	} {
		seeded := existing + "YACHT_NETWORK_ISOLATION=" + set.mode + "\nYACHT_EGRESS_ALLOW=" + set.allow + "\n"
		if err := os.WriteFile(envFile, []byte(seeded), 0o600); err != nil {
			t.Fatal(err)
		}
		rendered := renderEnv(t, lib, envFile)
		if got := valueOf(rendered, "YACHT_NETWORK_ISOLATION"); got != set.mode {
			t.Errorf("a re-run changed YACHT_NETWORK_ISOLATION=%s to %q", set.mode, got)
		}
		if got := valueOf(rendered, "YACHT_EGRESS_ALLOW"); got != set.allow {
			t.Errorf("a re-run changed YACHT_EGRESS_ALLOW=%s to %q", set.allow, got)
		}
	}
}

// The upgrader never writes the environment file, and tells an install not on
// strict what that leaves open and how to change it — once, and not to one
// that chose strict or off.
func TestUpgraderRecommendsStrictAndChangesNothing(t *testing.T) {
	lib := sourceableScript(t, "upgrade.sh")
	dir := t.TempDir()
	envFile := filepath.Join(dir, "yacht.env")

	for _, tc := range []struct {
		name, file string
		note       bool
	}{
		{"unset", "YACHT_ADDR=:8080\n", true},
		{"team", "YACHT_ADDR=:8080\nYACHT_NETWORK_ISOLATION=team\n", true},
		{"strict", "YACHT_ADDR=:8080\nYACHT_NETWORK_ISOLATION=strict\n", false},
		{"off", "YACHT_ADDR=:8080\nYACHT_NETWORK_ISOLATION=off\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(envFile, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			cmd := exec.Command("sh", "-c", `set -eu; . "$1"; ENV_FILE="$2"; isolation_note`, "sh", lib, envFile)
			cmd.Stdout, cmd.Stderr = &out, &errb
			if err := cmd.Run(); err != nil {
				t.Fatalf("isolation_note: %v\n%s", err, errb.String())
			}
			said := out.String()
			if tc.note {
				for _, want := range []string{"cloud metadata", "YACHT_NETWORK_ISOLATION=strict", envFile, "systemctl restart yacht"} {
					if !strings.Contains(said, want) {
						t.Errorf("the note does not say %q:\n%s", want, said)
					}
				}
			} else if said != "" {
				t.Errorf("an install on %s was told:\n%s", tc.name, said)
			}
			after, err := os.ReadFile(envFile)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != tc.file {
				t.Errorf("the upgrader changed the environment file:\n%s", after)
			}
		})
	}
}

// isolationLoaded is the isolation mode the engine reads out of an env file,
// as systemd would hand it over. A line that is absent is empty here, which
// the engine reads as it reads one unset.
func isolationLoaded(t *testing.T, file string) orchestrator.IsolationMode {
	t.Helper()
	t.Setenv("YACHT_DATABASE_URL", "postgres://localhost/yacht?sslmode=disable")
	t.Setenv("YACHT_NETWORK_ISOLATION", valueOf(file, "YACHT_NETWORK_ISOLATION"))
	t.Setenv("YACHT_EGRESS_ALLOW", valueOf(file, "YACHT_EGRESS_ALLOW"))
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	iso, err := c.Isolation()
	if err != nil {
		t.Fatalf("Isolation: %v", err)
	}
	return iso.Mode
}
