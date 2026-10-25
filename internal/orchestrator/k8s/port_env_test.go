package k8s

import (
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// An app is told its port as PORT, unless it has none or says PORT itself.
func TestPortEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec orchestrator.AppSpec
		want string
		ok   bool
	}{
		{"a port", orchestrator.AppSpec{Port: 4000}, "4000", true},
		{"no port", orchestrator.AppSpec{}, "", false},
		{"its own PORT", orchestrator.AppSpec{Port: 4000, Env: map[string]string{"PORT": "3000"}}, "", false},
		{"PORT as a secret", orchestrator.AppSpec{Port: 4000, Secrets: map[string]string{"PORT": "3000"}}, "", false},
	} {
		if got, ok := portEnv(tc.spec); got != tc.want || ok != tc.ok {
			t.Errorf("%s: portEnv = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
