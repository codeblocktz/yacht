package k8s

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
)

// Pod Security Admission labels applied to every namespace the engine creates.
//
// This is the enforcement half of the security posture. The restricted security
// context below is what the engine *asks* for; these labels are what the
// cluster *enforces*, including against anything that reaches the API by
// another path. Without them, the absence of privileged pods rests entirely on
// the control plane never offering the field — which is a policy, not a
// boundary.
const (
	psaEnforce        = "pod-security.kubernetes.io/enforce"
	psaEnforceVersion = "pod-security.kubernetes.io/enforce-version"
	psaAudit          = "pod-security.kubernetes.io/audit"
	psaWarn           = "pod-security.kubernetes.io/warn"

	psaLevel = "restricted"
)

func psaLabels() map[string]string {
	return map[string]string{
		psaEnforce:        psaLevel,
		psaEnforceVersion: "latest",
		psaAudit:          psaLevel,
		psaWarn:           psaLevel,
	}
}

// tmpVolumeName is an emptyDir mounted at /tmp.
//
// A read-only root filesystem breaks a surprising number of otherwise
// well-behaved images, almost always because something writes to /tmp. Mounting
// a writable /tmp unconditionally means the secure default stays usable, so
// fewer people reach for the WritableRootFilesystem escape hatch.
const tmpVolumeName = "tmp"

// restrictedContainerSecurityContext returns the container-level hardening the
// engine applies to every workload.
//
// There is no code path that weakens this beyond the documented
// WritableRootFilesystem flag. That is the point: the caller cannot ask for a
// privileged container because the request has nowhere to go.
func restrictedContainerSecurityContext(writableRoot bool) *corev1ac.SecurityContextApplyConfiguration {
	return corev1ac.SecurityContext().
		WithRunAsNonRoot(true).
		WithAllowPrivilegeEscalation(false).
		WithPrivileged(false).
		WithReadOnlyRootFilesystem(!writableRoot).
		WithCapabilities(corev1ac.Capabilities().WithDrop(corev1.Capability("ALL"))).
		WithSeccompProfile(
			corev1ac.SeccompProfile().WithType(corev1.SeccompProfileTypeRuntimeDefault),
		)
}

// restrictedPodSecurityContext returns the pod-level counterpart.
func restrictedPodSecurityContext(
	spec orchestrator.AppSpec,
) *corev1ac.PodSecurityContextApplyConfiguration {
	sc := corev1ac.PodSecurityContext().
		WithRunAsNonRoot(true).
		WithSeccompProfile(
			corev1ac.SeccompProfile().WithType(corev1.SeccompProfileTypeRuntimeDefault),
		)

	// Set only when a source knows the answer. Guessing a uid for an arbitrary
	// image gives a container that starts and then cannot read its own files,
	// which is a worse failure than refusing to start at all.
	if spec.RunAsUser > 0 {
		sc = sc.WithRunAsUser(spec.RunAsUser).WithRunAsGroup(spec.RunAsUser)
	}
	// Only meaningful with storage: it exists to hand a non-root process
	// ownership of a claim that was provisioned owned by root.
	if spec.FSGroup > 0 && len(spec.Volumes) > 0 {
		sc = sc.WithFSGroup(spec.FSGroup)
	}
	return sc
}
