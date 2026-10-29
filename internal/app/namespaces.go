package app

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// NamespacedApp is an app as the install's stores know it: by the namespace
// its pods run in and its traffic is counted under, with what a meter needs
// to charge for it.
type NamespacedApp struct {
	ID        uuid.UUID
	OwnerID   string
	ProjectID uuid.UUID // zero for an app on no project's canvas
	Name      string
	Namespace string
}

// AllNamespaces is every app on the install with its namespace, by owner and
// name.
//
// Every team's, unlike everything else here a request reaches: it is for the
// operator's meter, which attributes what a store counted per namespace to
// the app and team it belongs to.
func (s *Service) AllNamespaces(ctx context.Context) ([]NamespacedApp, error) {
	rows, err := s.q.ListAppNamespaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: list namespaces: %w", err)
	}
	out := make([]NamespacedApp, 0, len(rows))
	for _, r := range rows {
		out = append(out, NamespacedApp{
			ID: r.ID, OwnerID: r.OwnerID, ProjectID: r.ProjectID.Bytes,
			Name: r.Name, Namespace: r.Namespace,
		})
	}
	return out, nil
}
