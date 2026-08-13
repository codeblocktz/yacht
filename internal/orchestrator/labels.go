package orchestrator

// Label and annotation keys applied to every object the engine manages.
//
// These are part of the engine's public contract with the cluster: once
// workloads are running, changing them is not a rename but a migration.
// Kubernetes Deployment selectors are immutable, so a change to SelectorLabels
// in particular means deleting and recreating every Deployment in the fleet.
// Decide these before the first real workload, not after.
const (
	// LabelManagedBy marks objects the engine owns, so cleanup and queries can
	// safely ignore everything else in a cluster.
	LabelManagedBy = "app.kubernetes.io/managed-by"

	// LabelName is the conventional Kubernetes app name.
	LabelName = "app.kubernetes.io/name"

	// LabelApp is the engine's own stable handle on a workload. This is the
	// selector key.
	LabelApp = "yacht/app"

	// LabelOwner records the owning principal. Deliberately NOT part of the
	// selector: selectors are immutable, and keeping ownership out of them
	// leaves room to reassign a workload without recreating it.
	LabelOwner = "yacht/owner-id"

	// ManagedByValue is the value of LabelManagedBy.
	ManagedByValue = "yacht"
)

// Annotation keys.
const (
	// AnnotationRevision changes on every apply, which is what triggers a
	// rollout when the pod template is otherwise unchanged.
	AnnotationRevision = "yacht/revision"

	// These two annotations are the database convergence key observed by the
	// app reconciler.
	AnnotationReleaseID     = "yacht/release-id"
	AnnotationConfigVersion = "yacht/config-version"
)

// FieldManager identifies the engine to Kubernetes server-side apply. Using a
// stable field manager is what lets repeated applies converge instead of
// fighting over fields.
const FieldManager = "yacht-engine"

// SelectorLabels returns the minimal, stable label set used to match a
// workload's pods.
//
// Keep this small. Every key added here becomes immutable for the life of the
// Deployment.
func SelectorLabels(name string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelApp:       name,
	}
}

// ObjectLabels returns the full label set for a managed object: the selector
// labels plus descriptive ones that are safe to change later.
func ObjectLabels(ref Ref) map[string]string {
	l := SelectorLabels(ref.Name)
	l[LabelName] = ref.Name
	l[LabelOwner] = string(ref.Owner)
	return l
}
