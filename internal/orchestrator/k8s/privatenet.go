package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// privateNetworkManager writes a namespace's project label and peers. Its own
// name, so the engine's apply of the namespace, which does not carry the
// peers, never takes them back.
const privateNetworkManager = "yacht-private-network"

// SyncPrivateNetwork brings one app's namespace onto its project's private
// network: the project it is labelled with and the peers it lets in, its
// siblings' aliases, and the isolation those make.
//
// Patched rather than applied, because a patch of a namespace that is not
// there fails where an apply would make one: a sync that overlaps an app's
// deletion must not bring its namespace back, empty and without the security
// labels EnsureNamespace gives it.
func (o *Orchestrator) SyncPrivateNetwork(ctx context.Context, p orchestrator.PrivateNetwork) error {
	if err := p.Validate(); err != nil {
		return err
	}
	ns, err := o.client.CoreV1().Namespaces().Get(ctx, p.Namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return orchestrator.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("k8s: read namespace %q: %w", p.Namespace, err)
	}
	// A namespace is the app's only while it says so: the backstop against a
	// caller grouping another team's namespace into a project.
	if owner := ns.Labels[orchestrator.LabelOwner]; owner != string(p.Owner) {
		return fmt.Errorf("k8s: namespace %q belongs to %q, not %q", p.Namespace, owner, p.Owner)
	}

	if ns, err = o.patchNetworkMeta(ctx, p); apierrors.IsNotFound(err) {
		return orchestrator.ErrNotFound
	} else if err != nil {
		return err
	}
	if err := o.syncAliases(ctx, p); err != nil {
		return err
	}
	return o.isolate(ctx, func(iso orchestrator.Isolation) *networkingv1ac.NetworkPolicyApplyConfiguration {
		return namespaceIsolation(ns, iso)
	})
}

// patchNetworkMeta writes the project label and the peers annotation, or
// removes either when there is none.
func (o *Orchestrator) patchNetworkMeta(ctx context.Context, p orchestrator.PrivateNetwork) (*corev1.Namespace, error) {
	var project, peers any // nil removes, in a merge patch
	if p.Project != "" {
		project = p.Project
	}
	if len(p.Peers) > 0 {
		sorted := slices.Clone(p.Peers)
		slices.Sort(sorted)
		peers = strings.Join(slices.Compact(sorted), ",")
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"labels":      map[string]any{orchestrator.LabelProject: project},
		"annotations": map[string]any{orchestrator.AnnotationPrivatePeers: peers},
	}})
	if err != nil {
		return nil, err
	}
	ns, err := o.client.CoreV1().Namespaces().Patch(ctx, p.Namespace, types.MergePatchType, body,
		metav1.PatchOptions{FieldManager: privateNetworkManager})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("k8s: label namespace %q with its project: %w", p.Namespace, err)
	}
	return ns, err
}

// syncAliases makes the namespace's aliases those asked for: an ExternalName
// Service for each, and none for a sibling that has gone. A Service that is
// not an alias — the app's own, a TCP proxy's — is never replaced, whatever
// an alias would be called: the app keeps its name, and the sibling it
// shadows is still reached by its full name.
func (o *Orchestrator) syncAliases(ctx context.Context, p orchestrator.PrivateNetwork) error {
	services := o.client.CoreV1().Services(p.Namespace)
	list, err := services.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("k8s: list services in %q: %w", p.Namespace, err)
	}
	aliases, taken := map[string]corev1.Service{}, map[string]bool{}
	for _, svc := range list.Items {
		if svc.Labels[orchestrator.LabelPrivateAlias] != "" {
			aliases[svc.Name] = svc
		} else {
			taken[svc.Name] = true
		}
	}

	want := map[string]bool{}
	for _, a := range p.Aliases {
		if taken[a.Name] {
			continue
		}
		want[a.Name] = true
		if have, ok := aliases[a.Name]; ok && have.Spec.Type == corev1.ServiceTypeExternalName && have.Spec.ExternalName == a.Host {
			continue
		}
		if _, err := services.Apply(ctx, aliasService(p, a), applyOpts()); err != nil {
			return fmt.Errorf("k8s: apply private name %s in %q: %w", a.Name, p.Namespace, err)
		}
	}
	for name := range aliases {
		if want[name] {
			continue
		}
		err := services.Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("k8s: remove private name %s in %q: %w", name, p.Namespace, err)
		}
	}
	return nil
}

// aliasService is a sibling's private name in a namespace. No ports: an
// ExternalName Service is only ever a name in DNS, and a client connects to
// whichever port the sibling listens on.
func aliasService(p orchestrator.PrivateNetwork, a orchestrator.PrivateAlias) *corev1ac.ServiceApplyConfiguration {
	return corev1ac.Service(a.Name, p.Namespace).
		WithLabels(map[string]string{
			orchestrator.LabelManagedBy:    orchestrator.ManagedByValue,
			orchestrator.LabelOwner:        string(p.Owner),
			orchestrator.LabelPrivateAlias: "true",
		}).
		WithSpec(corev1ac.ServiceSpec().
			WithType(corev1.ServiceTypeExternalName).
			WithExternalName(a.Host))
}

// namespacePeers are the other projects' namespaces a namespace lets in.
func namespacePeers(ns *corev1.Namespace) []string {
	var out []string
	for _, n := range strings.Split(ns.Annotations[orchestrator.AnnotationPrivatePeers], ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// namespaceIsolation is the policy for a team's namespace, as its labels and
// annotations describe it: its owner, its project and its peers. Read from
// the namespace wherever the policy is rebuilt — a deploy, a sync, a change
// of mode — so all three agree without being told.
func namespaceIsolation(ns *corev1.Namespace, iso orchestrator.Isolation) *networkingv1ac.NetworkPolicyApplyConfiguration {
	return teamIsolation(ns.Name, ns.Labels[orchestrator.LabelOwner],
		ns.Labels[orchestrator.LabelProject], namespacePeers(ns), iso)
}

var _ orchestrator.PrivateNetworker = (*Orchestrator)(nil)
