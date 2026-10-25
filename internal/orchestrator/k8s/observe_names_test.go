package k8s

import (
	"testing"

	"github.com/codeblocktz/yacht/internal/observe"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// The metrics store finds an app's requests by the name Traefik counts them
// under, which the sleeper reads here too. Two copies of one rule, held
// together: if the ingress's naming changes, both must.
func TestTheMetricsStoreNamesAnAppsTraefikServiceAsTheSleeperDoes(t *testing.T) {
	ref := orchestrator.Ref{Owner: "team-a", Namespace: "yacht-0123456789abcdef", Name: "web"}
	got := observe.TraefikService(observe.Service{Name: ref.Name, Namespace: ref.Namespace})
	if want := traefikServiceName(ref); got != want {
		t.Errorf("observe names it %q, the sleeper %q", got, want)
	}
}
