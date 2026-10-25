package orchestrator

import "context"

// Usage is what a namespace's containers are using at this moment.
type Usage struct {
	CPUMillis   int64
	MemoryBytes int64
}

// UsageReader is what an Orchestrator also implements to say what an app is
// using now, rather than what it asked for. The k8s orchestrator reads it from
// metrics-server, which keeps no history: one reading, about a minute old.
//
// The dashboard falls back to it where the install has no metrics store, so a
// project's Observability page still answers "what is busy" without one. An
// orchestrator that cannot say leaves the panel out.
type UsageReader interface {
	// NamespaceUsage is the summed use of every container in each of the
	// namespaces, by namespace. A namespace with nothing running is absent.
	NamespaceUsage(ctx context.Context, namespaces []string) (map[string]Usage, error)
}
