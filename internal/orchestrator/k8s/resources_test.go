package k8s

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// What an app is given is written on its pod, so the pod says what the
// dashboard says. Left to the namespace LimitRange, a namespace made before
// the defaults changed went on handing out 0.1 vCPU and 128 MiB to an app the
// dashboard said had no limit at all.
func TestAnAppIsGivenItsResourcesOnThePod(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		cpuReq, cpuLim, memReq, memLim string
		want                           [4]string // cpu request, cpu limit, memory request, memory limit
	}{
		{
			name: "nothing set is the defaults",
			want: [4]string{"100m", "500m", "128Mi", "512Mi"},
		},
		{
			name:   "what was set is kept",
			cpuReq: "250m", cpuLim: "2", memReq: "256Mi", memLim: "1Gi",
			want: [4]string{"250m", "2", "256Mi", "1Gi"},
		},
		{
			// The default request, not the limit as Kubernetes would make
			// it, so a limit raised for headroom does not also take the
			// whole of it from the node.
			name:   "a limit alone keeps the default request",
			cpuLim: "1", memLim: "2Gi",
			want: [4]string{"100m", "1", "128Mi", "2Gi"},
		},
		{
			// Kubernetes refuses a request above the limit, and the default
			// limit is the half nobody chose.
			name:   "a reservation above the default limit lifts it",
			cpuReq: "750m", memReq: "1Gi",
			want: [4]string{"750m", "750m", "1Gi", "1Gi"},
		},
		{
			name:   "a reservation below it leaves the default limit",
			cpuReq: "250m", memReq: "256Mi",
			want: [4]string{"250m", "500m", "256Mi", "512Mi"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			o, client := testOrchestrator(t)
			spec := testSpec()
			spec.CPURequest, spec.CPULimit = tc.cpuReq, tc.cpuLim
			spec.MemoryRequest, spec.MemoryLimit = tc.memReq, tc.memLim
			if err := o.ApplyApp(ctx, spec); err != nil {
				t.Fatalf("ApplyApp: %v", err)
			}
			dep, err := client.AppsV1().Deployments("yacht-demo").Get(ctx, "web", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			r := dep.Spec.Template.Spec.Containers[0].Resources
			got := [4]string{
				r.Requests.Cpu().String(), r.Limits.Cpu().String(),
				r.Requests.Memory().String(), r.Limits.Memory().String(),
			}
			if got != tc.want {
				t.Errorf("cpu %s/%s, memory %s/%s (request/limit); want %s/%s, %s/%s",
					got[0], got[1], got[2], got[3], tc.want[0], tc.want[1], tc.want[2], tc.want[3])
			}
		})
	}
}
