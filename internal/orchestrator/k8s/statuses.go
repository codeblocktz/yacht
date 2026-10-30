package k8s

import (
	"context"
	"errors"
	"fmt"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

var _ orchestrator.StatusLister = (*Orchestrator)(nil)

// AppStatuses reports many workloads' state as AppStatus does each one's,
// with a List of each kind per owner instead of a GET per workload.
//
// Every workload has a namespace of its own, so the lists are across
// namespaces, selected by the owner label every object Yacht writes has
// carried from the start; one that is not in them does not exist, as a GET
// would have said. Nothing is cached: every call reads the cluster, so a
// status is as fresh as AppStatus's. Deployments and CronJobs are read
// concurrently; pods only when a rollout is short of ready, in one more list.
func (o *Orchestrator) AppStatuses(
	ctx context.Context, refs []orchestrator.Ref,
) (map[orchestrator.Ref]orchestrator.AppStatus, error) {
	byOwner := map[orchestrator.OwnerID][]orchestrator.Ref{}
	for _, ref := range refs {
		if ref.Validate() != nil {
			continue
		}
		byOwner[ref.Owner] = append(byOwner[ref.Owner], ref)
	}

	out := make(map[orchestrator.Ref]orchestrator.AppStatus, len(refs))
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	for owner, refs := range byOwner {
		wg.Go(func() {
			got, err := o.ownerStatuses(ctx, owner, refs)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			for ref, status := range got {
				out[ref] = status
			}
		})
	}
	wg.Wait()
	// What could be read is returned with the error, so one owner the
	// cluster will not list does not blank every other one's services.
	return out, errors.Join(errs...)
}

// ownerStatuses reads one owner's workloads.
func (o *Orchestrator) ownerStatuses(
	ctx context.Context, owner orchestrator.OwnerID, refs []orchestrator.Ref,
) (map[orchestrator.Ref]orchestrator.AppStatus, error) {
	mine := metav1.ListOptions{LabelSelector: labels.SelectorFromSet(map[string]string{
		orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
		orchestrator.LabelOwner:     string(owner),
	}).String()}

	var (
		deps            *appsv1.DeploymentList
		crons           *batchv1.CronJobList
		depErr, cronErr error
		wg              sync.WaitGroup
	)
	wg.Go(func() { deps, depErr = o.client.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, mine) })
	wg.Go(func() { crons, cronErr = o.client.BatchV1().CronJobs(metav1.NamespaceAll).List(ctx, mine) })
	wg.Wait()
	if depErr != nil {
		return nil, fmt.Errorf("k8s: list deployments of %s: %w", owner, depErr)
	}
	if cronErr != nil {
		return nil, fmt.Errorf("k8s: list cron jobs of %s: %w", owner, cronErr)
	}

	depByName := make(map[types.NamespacedName]*appsv1.Deployment, len(deps.Items))
	for i := range deps.Items {
		d := &deps.Items[i]
		depByName[types.NamespacedName{Namespace: d.Namespace, Name: d.Name}] = d
	}
	cronByName := make(map[types.NamespacedName]*batchv1.CronJob, len(crons.Items))
	for i := range crons.Items {
		cj := &crons.Items[i]
		cronByName[types.NamespacedName{Namespace: cj.Namespace, Name: cj.Name}] = cj
	}

	out := make(map[orchestrator.Ref]orchestrator.AppStatus, len(refs))
	var short []orchestrator.Ref
	for _, ref := range refs {
		key := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
		if dep, ok := depByName[key]; ok {
			status := deploymentStatus(dep)
			if needsPodCheck(status) {
				short = append(short, ref)
			}
			out[ref] = status
			continue
		}
		// No Deployment is also what a workload on a schedule has.
		if cj, ok := cronByName[key]; ok {
			out[ref] = cronStatus(cj)
		}
	}

	if len(short) > 0 {
		// As AppStatus: pods that cannot be read leave the status as the
		// Deployment has it.
		pods, err := o.client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, mine)
		if err == nil {
			for _, ref := range short {
				selector := labels.SelectorFromSet(orchestrator.ObjectLabels(ref))
				var its []corev1.Pod
				for _, p := range pods.Items {
					if p.Namespace == ref.Namespace && selector.Matches(labels.Set(p.Labels)) {
						its = append(its, p)
					}
				}
				status := out[ref]
				key := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
				applyUnstartable(&status, its, crashLimit(depByName[key].Annotations))
				out[ref] = status
			}
		}
	}
	for ref, status := range out {
		out[ref] = finishStatus(status)
	}
	return out, nil
}
