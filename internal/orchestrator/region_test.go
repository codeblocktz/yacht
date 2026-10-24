package orchestrator

import (
	"strings"
	"testing"
)

// A region names DNS records and StorageClasses in the wrapper, so it is a DNS
// label: what a pool or a site may carry beyond that — dots, underscores — is
// refused, as is anything a shell would read as more than one word.
func TestValidateRegion(t *testing.T) {
	for _, ok := range []string{"", "fsn", "dar", "eu-1", "a", strings.Repeat("a", 63)} {
		if err := ValidateRegion(ok); err != nil {
			t.Errorf("ValidateRegion(%q) = %v; want valid", ok, err)
		}
	}
	for _, bad := range []string{
		"eu.central", "dar_1", "Dar", "-dar", "dar-", "dar 1", "dar;reboot", "$(id)",
		strings.Repeat("a", 64),
	} {
		if err := ValidateRegion(bad); err == nil {
			t.Errorf("ValidateRegion(%q) = nil; want refused", bad)
		}
	}
}
