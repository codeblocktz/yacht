package config

import (
	"os/exec"
	"testing"
)

// Keep the shell installer contract inside the ordinary Go test graph. This
// must fail, rather than skip, when sh is unavailable: a green release gate
// that did not exercise the root-facing scripts is not a successful check.
func TestInstallerCertManagerShellContracts(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("find sh for installer contract suite: %v", err)
	}

	cmd := exec.Command(sh, "../../tests/installer_cert_manager_test.sh")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installer cert-manager shell contracts: %v\n%s", err, out)
	}
	t.Logf("installer cert-manager shell contracts:\n%s", out)
}
