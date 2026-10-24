package config

import (
	"os/exec"
	"testing"
)

// The multi-site installer contract, run in the Go test graph for the reason
// the cert-manager one is: a release gate that skipped the root-facing script
// is not a pass.
func TestInstallerMultiSiteShellContracts(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("find sh for installer contract suite: %v", err)
	}

	cmd := exec.Command(sh, "../../tests/installer_multi_site_test.sh")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installer multi-site shell contracts: %v\n%s", err, out)
	}
	t.Logf("installer multi-site shell contracts:\n%s", out)
}
