package app

import (
	"context"
	"fmt"
	"time"

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

	// DeletedAt is when the app was deleted; zero for one that exists.
	DeletedAt time.Time
}

// AllNamespaces is every app on the install with its namespace, and every
// one deleted at or after since, by owner and name: what a store counted in
// a window that began at since may be any of theirs.
//
// One app to a namespace. A namespace is named for its team and the app's
// name, so an app deleted and made again under the same name has the same
// one; the app there now is given it, and of several deleted ones the last.
// Either way the team charged is the same.
//
// Every team's, unlike everything else here a request reaches: it is for the
// operator's meter, which attributes what a store counted per namespace to
// the app and team it belongs to.
func (s *Service) AllNamespaces(ctx context.Context, since time.Time) ([]NamespacedApp, error) {
	rows, err := s.q.ListAppNamespaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: list namespaces: %w", err)
	}
	gone, err := s.q.ListDeletedAppNamespaces(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("app: list deleted namespaces: %w", err)
	}
	out := make([]NamespacedApp, 0, len(rows)+len(gone))
	taken := map[string]bool{}
	for _, r := range rows {
		taken[r.Namespace] = true
		out = append(out, NamespacedApp{
			ID: r.ID, OwnerID: r.OwnerID, ProjectID: r.ProjectID.Bytes,
			Name: r.Name, Namespace: r.Namespace,
		})
	}
	// Latest deletion first within each name, so the first seen is kept.
	for _, r := range gone {
		if taken[r.Namespace] {
			continue
		}
		taken[r.Namespace] = true
		out = append(out, NamespacedApp{
			ID: r.ID, OwnerID: r.OwnerID, ProjectID: r.ProjectID.Bytes,
			Name: r.Name, Namespace: r.Namespace, DeletedAt: r.DeletedAt,
		})
	}
	return out, nil
}
