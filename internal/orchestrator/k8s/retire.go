package k8s

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

var _ orchestrator.NodeRetirer = (*Orchestrator)(nil)

// buildLabel marks a build's pod. A build is left to finish rather than
// evicted: it is never retried, so evicting one fails somebody's deploy.
const buildLabel = "yacht/build"

// RetirePlan reads what retiring a node involves.
//
// One list of each kind rather than a query per pod: the plan is read on every
// poll of the node page and every pass of the retirement, and on a busy node a
// query per pod is the difference between one round trip and hundreds.
func (o *Orchestrator) RetirePlan(ctx context.Context, node string) (orchestrator.RetirePlan, error) {
	nodes, err := o.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return orchestrator.RetirePlan{}, fmt.Errorf("k8s: list nodes: %w", err)
	}
	self := slices.IndexFunc(nodes.Items, func(n corev1.Node) bool { return n.Name == node })
	if self < 0 {
		return orchestrator.RetirePlan{}, fmt.Errorf("k8s: no node %q: %w", node, orchestrator.ErrNotFound)
	}
	pods, err := o.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return orchestrator.RetirePlan{}, fmt.Errorf("k8s: list pods: %w", err)
	}
	storage, err := o.readStorage(ctx)
	if err != nil {
		return orchestrator.RetirePlan{}, err
	}

	plan := orchestrator.RetirePlan{
		Node:     node,
		Since:    retiringSince(nodes.Items[self]),
		Cordoned: nodes.Items[self].Spec.Unschedulable,
	}
	here := podsOn(pods.Items, node)
	moving := classify(&plan, here, storage, nodes.Items)

	if err := o.fillApps(ctx, &plan); err != nil {
		return orchestrator.RetirePlan{}, err
	}
	plan.Room = retireRoom(nodes.Items, pods.Items, node, moving)
	return plan, nil
}

// podsOn is what a node is running: scheduled there, not finished, and not
// already on its way out.
func podsOn(pods []corev1.Pod, node string) []*corev1.Pod {
	var out []*corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName == node && active(p) {
			out = append(out, p)
		}
	}
	return out
}

// active reports whether a pod still occupies its node.
func active(p *corev1.Pod) bool {
	return p.DeletionTimestamp == nil &&
		p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed
}

// classify sorts a node's pods into what a retirement does with each, and
// returns the ones that will need room elsewhere.
//
// Storage first, because it overrides everything else: an app whose volume is
// on this machine is not restarted even though it is the engine's, since its
// replacement could only ever be placed back here.
func classify(
	plan *orchestrator.RetirePlan, here []*corev1.Pod, st storageIndex, nodes []corev1.Node,
) []*corev1.Pod {
	pinnedApps := map[string]bool{}
	var rest []*corev1.Pod
	for _, p := range here {
		if skipOnDrain(p) {
			continue
		}
		if pin, ok := st.pinned(p, plan.Node, nodes); ok {
			plan.Pinned = append(plan.Pinned, pin)
			if pin.App != "" {
				pinnedApps[p.Namespace+"/"+pin.App] = true
			}
			continue
		}
		rest = append(rest, p)
	}

	var moving []*corev1.Pod
	apps := map[string]*orchestrator.RetireApp{}
	for _, p := range rest {
		name := appOf(p)
		switch {
		case name != "" && pinnedApps[p.Namespace+"/"+name]:
			// A sibling of a pinned pod stays with it. Restarting the app
			// would restart the pinned pod too.
			continue
		case name != "":
			key := p.Namespace + "/" + name
			if apps[key] == nil {
				apps[key] = &orchestrator.RetireApp{Namespace: p.Namespace, Name: name}
			}
			apps[key].PodsHere++
		case p.Labels[buildLabel] == "true":
			// Left to finish. It holds nothing that needs room elsewhere.
			plan.Finishing = append(plan.Finishing, podRef(p))
			continue
		default:
			plan.Evict = append(plan.Evict, podRef(p))
		}
		moving = append(moving, p)
	}
	for _, a := range apps {
		plan.Apps = append(plan.Apps, *a)
	}
	return moving
}

func podRef(p *corev1.Pod) orchestrator.PodRef {
	return orchestrator.PodRef{Namespace: p.Namespace, Name: p.Name}
}

// appOf is the engine workload a pod belongs to, or "" for anything else.
//
// The labels alone are not enough: a build's pod carries the managed-by label
// too. Ownership by a ReplicaSet is what makes it a Deployment's pod, and a
// Deployment's pod is what a rolling restart can move.
func appOf(p *corev1.Pod) string {
	if p.Labels[orchestrator.LabelManagedBy] != orchestrator.ManagedByValue {
		return ""
	}
	for _, ref := range p.OwnerReferences {
		if ref.Kind == "ReplicaSet" {
			return p.Labels[orchestrator.LabelApp]
		}
	}
	return ""
}

// fillApps reads each app's Deployment: how it updates, and whether this
// retirement has already restarted it.
//
// Every engine Deployment is read, not only those with pods here, because an
// app this retirement has already moved has no pods here any more and still
// counts toward "N of M moved".
func (o *Orchestrator) fillApps(ctx context.Context, plan *orchestrator.RetirePlan) error {
	deps, err := o.client.AppsV1().Deployments("").List(ctx, metav1.ListOptions{
		LabelSelector: orchestrator.LabelManagedBy + "=" + orchestrator.ManagedByValue,
	})
	if err != nil {
		return fmt.Errorf("k8s: list deployments: %w", err)
	}

	token := plan.Token()
	byKey := map[string]int{}
	for i, a := range plan.Apps {
		byKey[a.Namespace+"/"+a.Name] = i
	}
	for i := range deps.Items {
		d := &deps.Items[i]
		key := d.Namespace + "/" + d.Name
		restarted := token != "" && d.Spec.Template.Annotations[orchestrator.AnnotationRetiredFrom] == token
		idx, here := byKey[key]
		if !here && !restarted {
			continue
		}
		if !here {
			plan.Apps = append(plan.Apps, orchestrator.RetireApp{Namespace: d.Namespace, Name: d.Name})
			idx = len(plan.Apps) - 1
		}
		a := &plan.Apps[idx]
		a.Owner = orchestrator.OwnerID(d.Labels[orchestrator.LabelOwner])
		a.Rolling = d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType
		a.Restarted = restarted
		a.RolledOut = rolledOut(d)
		a.Stuck = rolloutStuck(d)
	}

	slices.SortFunc(plan.Apps, func(a, b orchestrator.RetireApp) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return nil
}

// rolledOut is `kubectl rollout status` as a predicate: the controller has
// seen the latest template, every replica is on it and available, and none of
// the old ones remain.
func rolledOut(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	return s.ObservedGeneration >= d.Generation &&
		s.UpdatedReplicas >= want &&
		s.Replicas == s.UpdatedReplicas &&
		s.AvailableReplicas >= s.UpdatedReplicas
}

// rolloutStuck is why a rollout will not finish on its own, or "".
//
// Only the controller's own verdict counts. A rollout that is merely slow —
// an image still pulling — is not stuck, and moving on from it would start the
// next app's surge on top of this one's.
func rolloutStuck(d *appsv1.Deployment) string {
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse &&
			c.Reason == "ProgressDeadlineExceeded" {
			return c.Message
		}
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			return c.Message
		}
	}
	return ""
}

// retiringSince reads a node's retirement mark.
func retiringSince(n corev1.Node) time.Time {
	v, ok := n.Annotations[orchestrator.AnnotationRetiringSince]
	if !ok {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

// MarkRetiring records or clears a node's retirement mark.
func (o *Orchestrator) MarkRetiring(ctx context.Context, node string, since time.Time) error {
	var value any // nil clears the key in a merge patch
	if !since.IsZero() {
		value = strconv.FormatInt(since.Unix(), 10)
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{
			orchestrator.AnnotationRetiringSince: value,
		}},
	})
	if err != nil {
		return fmt.Errorf("k8s: encode retirement mark: %w", err)
	}
	_, err = o.client.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("k8s: no node %q: %w", node, orchestrator.ErrNotFound)
		}
		return fmt.Errorf("k8s: mark %s retiring: %w", node, err)
	}
	return nil
}

// RestartApp starts a rolling restart by writing the token into the pod
// template, which is all `kubectl rollout restart` does.
//
// A merge patch outside server-side apply, on purpose: the key is not one the
// engine's apply owns, so the next deploy neither removes it nor restarts the
// app a second time because of it.
func (o *Orchestrator) RestartApp(ctx context.Context, namespace, name, token string) error {
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
			"annotations": map[string]string{orchestrator.AnnotationRetiredFrom: token},
		}}},
	})
	if err != nil {
		return fmt.Errorf("k8s: encode restart: %w", err)
	}
	_, err = o.client.AppsV1().Deployments(namespace).
		Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("k8s: no app %s/%s: %w", namespace, name, orchestrator.ErrNotFound)
		}
		return fmt.Errorf("k8s: restart %s/%s: %w", namespace, name, err)
	}
	return nil
}

// EvictPod asks one pod to leave.
func (o *Orchestrator) EvictPod(ctx context.Context, namespace, name string) error {
	_, err := o.evict(ctx, namespace, name)
	return err
}

// retireRoom works out whether the other nodes can hold what is moving.
//
// Placed pod by pod, largest first, rather than compared in total: two nodes
// with 1 GiB free each do not hold a pod that asks for 2, and totals alone
// would say they do. Still an estimate — the scheduler also weighs affinity,
// spreading and port conflicts — but one that errs toward refusing.
func retireRoom(nodes []corev1.Node, pods []corev1.Pod, node string, moving []*corev1.Pod) orchestrator.RetireRoom {
	var room orchestrator.RetireRoom
	free := freeByNode(nodes, pods, node)
	for _, f := range free {
		room.FreeCPUMillis += f.cpu
		room.FreeMemBytes += f.mem
	}

	type need struct {
		name     string
		cpu, mem int64
	}
	needs := make([]need, 0, len(moving))
	for _, p := range moving {
		cpu, mem := podRequests(p)
		room.NeedCPUMillis += cpu
		room.NeedMemBytes += mem
		needs = append(needs, need{p.Namespace + "/" + p.Name, cpu, mem})
	}
	slices.SortStableFunc(needs, func(a, b need) int {
		return cmp.Or(cmp.Compare(b.mem, a.mem), cmp.Compare(b.cpu, a.cpu))
	})

	for _, n := range needs {
		i := slices.IndexFunc(free, func(f resources) bool { return f.cpu >= n.cpu && f.mem >= n.mem })
		if i < 0 {
			room.Unplaced = n.name
			break
		}
		free[i].cpu -= n.cpu
		free[i].mem -= n.mem
	}
	return room
}

type resources struct{ cpu, mem int64 }

// freeByNode is what each other node could still accept.
//
// A node counts only if it is ready, accepting work, and carries no taint that
// keeps ordinary pods off. Some pods tolerate such taints, but counting room
// they cannot use would pass a check the scheduler then fails.
func freeByNode(nodes []corev1.Node, pods []corev1.Pod, node string) []resources {
	used := map[string]resources{}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName == "" || !active(p) {
			continue
		}
		cpu, mem := podRequests(p)
		u := used[p.Spec.NodeName]
		used[p.Spec.NodeName] = resources{u.cpu + cpu, u.mem + mem}
	}

	var out []resources
	for _, n := range nodes {
		if n.Name == node || n.Spec.Unschedulable || !nodeReady(n) || repels(n) {
			continue
		}
		u := used[n.Name]
		out = append(out, resources{
			cpu: n.Status.Allocatable.Cpu().MilliValue() - u.cpu,
			mem: n.Status.Allocatable.Memory().Value() - u.mem,
		})
	}
	return out
}

// repels reports whether a node keeps ordinary pods off with a taint.
func repels(n corev1.Node) bool {
	for _, t := range n.Spec.Taints {
		if t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}

// podRequests is what the scheduler reserves for a pod: its containers
// together, or its largest init container if that is more, plus overhead.
func podRequests(p *corev1.Pod) (cpu, mem int64) {
	for _, c := range p.Spec.Containers {
		cpu += c.Resources.Requests.Cpu().MilliValue()
		mem += c.Resources.Requests.Memory().Value()
	}
	for _, c := range p.Spec.InitContainers {
		cpu = max(cpu, c.Resources.Requests.Cpu().MilliValue())
		mem = max(mem, c.Resources.Requests.Memory().Value())
	}
	cpu += p.Spec.Overhead.Cpu().MilliValue()
	mem += p.Spec.Overhead.Memory().Value()
	return cpu, mem
}
