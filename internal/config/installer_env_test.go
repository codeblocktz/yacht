package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/secret"
)

// The installer rewrites /etc/yacht/yacht.env from scratch on every run, so a
// value it does not read back is a value it silently drops. For a retired
// encryption key that is not an inconvenience: the operator re-runs the
// installer mid-rotation, the old key goes, and every value not yet re-sealed
// becomes unopenable with nothing having reported an error.
//
// The check is idempotence, not a list of variables to remember. Rendering the
// file, feeding it back, and rendering again must produce the same bytes —
// which fails the moment anything is written without being preserved, for this
// variable and every one added later.
//
// This drives the installer's own render_env, so it tests the shipped code
// rather than a copy of its rules. What it cannot cover is the rest of a run:
// K3s, Postgres, useradd and the systemd unit all need a machine. Those stay
// verified by hand on a fresh Debian box — the CI shellcheck and `sh -n` gates
// are the only other automated coverage install.sh has.
func TestInstallerPreservesRotationKeysAcrossRuns(t *testing.T) {
	lib := sourceableInstaller(t)
	dir := t.TempDir()
	envFile := filepath.Join(dir, "yacht.env")

	// Base64 with the characters that make a naive reader lose the value: '+',
	// '/' and the padding '='. The installer reads these back through sed, and
	// a delimiter chosen badly would truncate a key rather than fail loudly.
	retiredOne := base64.StdEncoding.EncodeToString(append(bytes.Repeat([]byte{0xfb, 0xef, 0xbe}, 10), 0x01, 0x02))
	retiredTwo := base64.StdEncoding.EncodeToString(append(bytes.Repeat([]byte{0xff, 0xff, 0xff}, 10), 0x03, 0x04))
	activeKey := base64.StdEncoding.EncodeToString(append(bytes.Repeat([]byte{0x3e, 0x7f, 0xc1}, 10), 0x05, 0x06))
	previous := retiredOne + "," + retiredTwo

	const (
		token = "0123456789abcdef0123456789abcdef0123456789abcdef"
		email = "owner@example.test"
	)

	seeded := strings.Join([]string{
		"YACHT_DATABASE_URL=postgres://yacht:old@127.0.0.1:5432/yacht?sslmode=disable",
		"YACHT_AUTH_TOKEN=" + token,
		"YACHT_SECRET_KEY=" + activeKey,
		"YACHT_SECRET_KEY_PREVIOUS=" + previous,
		"YACHT_OWNER_EMAIL=" + email,
		"",
	}, "\n")
	if err := os.WriteFile(envFile, []byte(seeded), 0o600); err != nil {
		t.Fatalf("seed the env file: %v", err)
	}

	first := renderEnv(t, lib, envFile)
	for name, want := range map[string]string{
		"YACHT_SECRET_KEY":          activeKey,
		"YACHT_SECRET_KEY_PREVIOUS": previous,
		"YACHT_AUTH_TOKEN":          token,
		"YACHT_OWNER_EMAIL":         email,
	} {
		if got := valueOf(first, name); got != want {
			t.Errorf("%s came back %q, want %q — re-running the installer changed it",
				name, got, want)
		}
	}

	// The second run, reading what the first one wrote. Anything the installer
	// emits but does not carry across disappears here.
	if err := os.WriteFile(envFile, []byte(first), 0o600); err != nil {
		t.Fatalf("write the rendered file back: %v", err)
	}
	if second := renderEnv(t, lib, envFile); second != first {
		t.Errorf("a second run produced a different file — something written is not read "+
			"back, so it is dropped on every re-run\n--- first ---\n%s\n--- second ---\n%s",
			first, second)
	}

	// And the preserved list has to be usable by the thing that consumes it,
	// not merely equal to itself: the file is a systemd EnvironmentFile, so it
	// reaches the keeper through Load.
	t.Setenv("YACHT_SECRET_KEY", valueOf(first, "YACHT_SECRET_KEY"))
	t.Setenv("YACHT_SECRET_KEY_PREVIOUS", valueOf(first, "YACHT_SECRET_KEY_PREVIOUS"))
	t.Setenv("YACHT_DATABASE_URL", valueOf(first, "YACHT_DATABASE_URL"))
	c, err := Load()
	if err != nil {
		t.Fatalf("Load from the file the installer wrote: %v", err)
	}
	keeper, err := secret.NewKeeper(c.SecretKey, c.SecretKeyPrevious...)
	if err != nil {
		t.Fatalf("NewKeeper from the file the installer wrote: %v", err)
	}
	if got := len(keeper.KeyIDs()); got != 3 {
		t.Errorf("the keeper holds %d keys, want the active one and both retired", got)
	}
}

// A fresh box has no file to preserve anything from. The active key is minted
// and the retired list must come out empty rather than absent or malformed —
// startup refuses retired keys with no active key, and it must not be the
// installer that puts an install in that state.
func TestInstallerMintsAKeyAndLeavesTheRetiredListEmpty(t *testing.T) {
	lib := sourceableInstaller(t)
	envFile := filepath.Join(t.TempDir(), "yacht.env")

	rendered := renderEnv(t, lib, envFile)

	if got := valueOf(rendered, "YACHT_SECRET_KEY_PREVIOUS"); got != "" {
		t.Errorf("a fresh install has YACHT_SECRET_KEY_PREVIOUS=%q, want it empty", got)
	}
	if !strings.Contains(rendered, "YACHT_SECRET_KEY_PREVIOUS=") {
		t.Error("a fresh install omits YACHT_SECRET_KEY_PREVIOUS entirely, so an operator " +
			"replacing a key has nowhere obvious to put the old one")
	}

	key, err := base64.StdEncoding.DecodeString(valueOf(rendered, "YACHT_SECRET_KEY"))
	if err != nil {
		t.Fatalf("the minted key is not base64: %v", err)
	}
	if len(key) != 32 {
		t.Errorf("the minted key is %d bytes, want 32", len(key))
	}
}

// sourceableInstaller writes install.sh up to its entrypoint to a temp file, so
// a test can call one function out of it without provisioning anything.
//
// The cut is on `main "$@"`, which the installer keeps as its last line so a
// truncated download runs nothing. If that line ever moves this fails rather
// than sourcing a script that would install K3s on the machine running the
// tests.
func sourceableInstaller(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH to run the installer with")
	}

	src, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	const entrypoint = "\nmain \"$@\"\n"
	cut := strings.Index(string(src), entrypoint)
	if cut == -1 {
		t.Fatal(`install.sh no longer ends with main "$@" — this test refuses to source it ` +
			`rather than risk executing the installer`)
	}

	lib := filepath.Join(t.TempDir(), "installer.sh")
	if err := os.WriteFile(lib, src[:cut], 0o600); err != nil {
		t.Fatalf("write the sourceable installer: %v", err)
	}
	return lib
}

// renderEnv runs the installer's render_env against one env file.
func renderEnv(t *testing.T, lib, envFile string) string {
	t.Helper()

	// The globals render_env reads, pinned so the only thing that varies
	// between two runs is what the installer decided to carry across.
	script := `
set -eu
. "$1"
ENV_FILE="$2"
DATABASE_URL='postgres://yacht:pw@127.0.0.1:5432/yacht?sslmode=disable'
KUBECONFIG_DST='/etc/yacht/kubeconfig'
PORT=8080
ROTATE_TOKEN=no
render_env
`
	var out, errb bytes.Buffer
	cmd := exec.Command("sh", "-c", script, "sh", lib, envFile)
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("render_env: %v\n%s", err, errb.String())
	}
	return out.String()
}

// valueOf reads one variable out of a rendered file, the way env_get does.
func valueOf(rendered, key string) string {
	for _, line := range strings.Split(rendered, "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return v
		}
	}
	return ""
}
