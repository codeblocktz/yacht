package app

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Workspace is a team's projects and apps as the dashboard's chrome needs
// them: the sidebar's project tree, the command palette's index, the count of
// deploys in flight, and the facts the getting-started checklist is computed
// from.
//
// Read in three queries whatever the team's size, and without asking the
// cluster anything. The chrome is drawn on every page, and an app's live
// status is a round trip to the cluster per app — which the canvas and the
// app's own page pay once, and a sidebar would pay on every navigation. So an
// app's State here is what its records say. See AppState.
type Workspace struct {
	Projects []WorkspaceProject

	// Unplaced are apps with no project: ones that predate projects, or whose
	// project was deleted. Projects() adopts them into the default project on
	// its next read; until then they are still the team's apps and still
	// searchable.
	Unplaced []WorkspaceApp

	// LiveDeploys is how many deployment operations are queued or running.
	LiveDeploys int

	Onboarding Onboarding
}

// WorkspaceProject is one project and the apps on its canvas, by name.
type WorkspaceProject struct {
	ID   uuid.UUID
	Slug string
	Name string
	Apps []WorkspaceApp
}

// WorkspaceApp is one app, with the state its records put it in.
type WorkspaceApp struct {
	Name  string
	State AppState
}

// AppState is how an app stands according to the database: what the
// dashboard can say about it without asking the cluster.
//
// Deliberately not orchestrator.Phase. A phase is observed; this is recorded,
// and the two can disagree — a release can be serving while its pods crash.
// Naming them differently keeps a sidebar dot from claiming to be a health
// check.
type AppState string

const (
	// AppDeploying has an operation queued or running.
	AppDeploying AppState = "deploying"
	// AppServing has a release in service and nothing wrong on record.
	AppServing AppState = "serving"
	// AppDeployFailed is serving an earlier release: its last deploy failed,
	// and the change somebody made never took.
	AppDeployFailed AppState = "deploy-failed"
	// AppFailed has never served: every attempt so far failed.
	AppFailed AppState = "failed"
	// AppStopped is scaled to zero.
	AppStopped AppState = "stopped"
	// AppIdle has never been deployed and nothing is under way.
	AppIdle AppState = "idle"
)

// appStateOf reads an app's state off its records.
//
// Order matters. A deploy in flight is what somebody is waiting on, so it
// wins over everything; a stopped app is stopped whatever its history says;
// only then does the last attempt's outcome say anything.
func appStateOf(deploying bool, replicas int32, serving bool, lastDeploy string) AppState {
	switch {
	case deploying:
		return AppDeploying
	case replicas == 0:
		return AppStopped
	case lastDeploy == DeployFailed && serving:
		return AppDeployFailed
	case lastDeploy == DeployFailed:
		return AppFailed
	case serving:
		return AppServing
	}
	return AppIdle
}

// Onboarding is what the getting-started checklist is computed from. Every
// field but Dismissed is a fact about the team's own data, so a step cannot be
// marked done without having been done.
type Onboarding struct {
	HasApps            bool
	HasSucceededDeploy bool
	HasCustomDomain    bool

	// Dismissed is the team having put the checklist away.
	Dismissed bool
}

// Workspace reads a team's projects, apps and checklist facts.
func (s *Service) Workspace(ctx context.Context, ownerID string) (Workspace, error) {
	projects, err := s.q.WorkspaceProjects(ctx, ownerID)
	if err != nil {
		return Workspace{}, fmt.Errorf("app: workspace projects: %w", err)
	}
	apps, err := s.q.WorkspaceApps(ctx, ownerID)
	if err != nil {
		return Workspace{}, fmt.Errorf("app: workspace apps: %w", err)
	}
	facts, err := s.q.WorkspaceFacts(ctx, ownerID)
	if err != nil {
		return Workspace{}, fmt.Errorf("app: workspace facts: %w", err)
	}
	return assembleWorkspace(projects, apps, facts), nil
}

// assembleWorkspace hangs each app off its project. Split from Workspace so
// the placing is testable without a database.
func assembleWorkspace(
	projects []dbgen.WorkspaceProjectsRow, apps []dbgen.WorkspaceAppsRow, facts dbgen.WorkspaceFactsRow,
) Workspace {
	w := Workspace{
		Projects:    make([]WorkspaceProject, 0, len(projects)),
		LiveDeploys: int(facts.LiveDeploys),
		Onboarding: Onboarding{
			HasApps:            facts.HasApps,
			HasSucceededDeploy: facts.HasSucceededDeploy,
			HasCustomDomain:    facts.HasCustomDomain,
			Dismissed:          facts.OnboardingDismissed,
		},
	}
	index := make(map[uuid.UUID]int, len(projects))
	for i, p := range projects {
		index[p.ID] = i
		w.Projects = append(w.Projects, WorkspaceProject{ID: p.ID, Slug: p.Slug, Name: p.Name})
	}
	for _, a := range apps {
		entry := WorkspaceApp{
			Name:  a.Name,
			State: appStateOf(a.Deploying, a.Replicas, a.Serving, a.LastDeploy),
		}
		if a.ProjectID.Valid {
			if i, ok := index[uuid.UUID(a.ProjectID.Bytes)]; ok {
				w.Projects[i].Apps = append(w.Projects[i].Apps, entry)
				continue
			}
		}
		w.Unplaced = append(w.Unplaced, entry)
	}
	return w
}

// DismissOnboarding puts the getting-started checklist away for the whole
// team. There is no undo: every step it would show is also reachable from the
// page it links to, and a checklist that can be summoned back is one more
// control nobody looks for.
func (s *Service) DismissOnboarding(ctx context.Context, ownerID string) error {
	if err := s.q.DismissOnboarding(ctx, ownerID); err != nil {
		return fmt.Errorf("app: dismiss onboarding: %w", err)
	}
	return nil
}
