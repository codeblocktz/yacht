package k8s

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Storage that holds a pod on its node.
//
// K3s provisions volumes with local-path by default: a directory on the
// machine that asked for it, and a PersistentVolume whose node affinity names
// that machine. A pod using one can only ever run there. Evicting it does not
// move it — the replacement sits Pending until the node takes work again — and
// removing the node strands the data. Found here by the affinity rather than by
// the provisioner's name, so any node-local storage is caught, not only K3s's.

// storageIndex is every claim and volume in the cluster, read once per plan.
type storageIndex struct {
	claims  map[string]*corev1.PersistentVolumeClaim // namespace/name
	volumes map[string]*corev1.PersistentVolume
}

func (o *Orchestrator) readStorage(ctx context.Context) (storageIndex, error) {
	pvcs, err := o.client.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return storageIndex{}, fmt.Errorf("k8s: list volume claims: %w", err)
	}
	pvs, err := o.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return storageIndex{}, fmt.Errorf("k8s: list volumes: %w", err)
	}
	idx := storageIndex{
		claims:  make(map[string]*corev1.PersistentVolumeClaim, len(pvcs.Items)),
		volumes: make(map[string]*corev1.PersistentVolume, len(pvs.Items)),
	}
	for i := range pvcs.Items {
		c := &pvcs.Items[i]
		idx.claims[c.Namespace+"/"+c.Name] = c
	}
	for i := range pvs.Items {
		idx.volumes[pvs.Items[i].Name] = &pvs.Items[i]
	}
	return idx, nil
}

// pinned reports whether a pod mounts a volume that only this node can serve.
//
// "Only this node" rather than "this node": a volume whose affinity names a
// zone holding several machines can follow its pod to another of them, and
// counting it as pinned would leave an app behind that could have moved.
func (s storageIndex) pinned(
	p *corev1.Pod, node string, nodes []corev1.Node,
) (orchestrator.PinnedPod, bool) {
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		claim := s.claims[p.Namespace+"/"+v.PersistentVolumeClaim.ClaimName]
		if claim == nil || claim.Spec.VolumeName == "" {
			continue
		}
		pv := s.volumes[claim.Spec.VolumeName]
		if pv == nil || !onlyNode(pv, node, nodes) {
			continue
		}
		return orchestrator.PinnedPod{
			Namespace: p.Namespace,
			Name:      p.Name,
			App:       p.Labels[orchestrator.LabelApp],
			Claim:     claim.Name,
			SizeBytes: claimSize(claim, pv),
		}, true
	}
	return orchestrator.PinnedPod{}, false
}

// onlyNode reports whether a volume's affinity admits this node and no other.
func onlyNode(pv *corev1.PersistentVolume, node string, nodes []corev1.Node) bool {
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return false
	}
	terms := pv.Spec.NodeAffinity.Required.NodeSelectorTerms
	admits := func(n corev1.Node) bool {
		return slices.ContainsFunc(terms, func(t corev1.NodeSelectorTerm) bool { return termMatches(t, n) })
	}
	here := false
	for _, n := range nodes {
		if !admits(n) {
			continue
		}
		if n.Name != node {
			return false
		}
		here = true
	}
	return here
}

// claimSize is how big the volume is, preferring what was provisioned.
func claimSize(c *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) int64 {
	if q, ok := c.Status.Capacity[corev1.ResourceStorage]; ok {
		return q.Value()
	}
	if q, ok := pv.Spec.Capacity[corev1.ResourceStorage]; ok {
		return q.Value()
	}
	return c.Spec.Resources.Requests.Storage().Value()
}

// termMatches evaluates one node selector term: every expression and field
// requirement must hold. An empty term matches nothing, as the API defines.
func termMatches(t corev1.NodeSelectorTerm, n corev1.Node) bool {
	if len(t.MatchExpressions) == 0 && len(t.MatchFields) == 0 {
		return false
	}
	for _, r := range t.MatchExpressions {
		v, has := n.Labels[r.Key]
		if !requirementHolds(r, v, has) {
			return false
		}
	}
	for _, r := range t.MatchFields {
		// metadata.name is the only field the API accepts here.
		if r.Key != "metadata.name" || !requirementHolds(r, n.Name, true) {
			return false
		}
	}
	return true
}

func requirementHolds(r corev1.NodeSelectorRequirement, v string, has bool) bool {
	switch r.Operator {
	case corev1.NodeSelectorOpIn:
		return has && slices.Contains(r.Values, v)
	case corev1.NodeSelectorOpNotIn:
		return !has || !slices.Contains(r.Values, v)
	case corev1.NodeSelectorOpExists:
		return has
	case corev1.NodeSelectorOpDoesNotExist:
		return !has
	case corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt:
		if !has || len(r.Values) != 1 {
			return false
		}
		got, err1 := strconv.ParseInt(v, 10, 64)
		want, err2 := strconv.ParseInt(r.Values[0], 10, 64)
		if err1 != nil || err2 != nil {
			return false
		}
		if r.Operator == corev1.NodeSelectorOpGt {
			return got > want
		}
		return got < want
	}
	return false
}
