package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/codeblocktz/yacht/internal/github"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// The GitHub App's one webhook.
//
// Every installation's events arrive here, signed with the App's webhook
// secret. A push deploys each app built from the repository — found by the
// repository's id, so a rename changes nothing — whose branch it moved, by
// handing it to the same actOnPush an app's own webhook uses: the branch
// check, the admission, the slot a push waits in behind a deploy in flight.
// Installation events record what GitHub took away, so the pages say so and
// builds stop, without deleting anything.

// ErrGitHubDeliveryInvalid is a delivery with no X-GitHub-Delivery id, or
// one too long to be GitHub's.
var ErrGitHubDeliveryInvalid = errors.New("app: a GitHub delivery needs its X-GitHub-Delivery id")

// GitHubDelivery is one delivery to the App's webhook, as the web layer
// received it.
type GitHubDelivery struct {
	// ID is X-GitHub-Delivery, Event X-GitHub-Event and Signature
	// X-Hub-Signature-256.
	ID        string
	Event     string
	Signature string
	Body      []byte
}

// DeliverGitHub acts on one delivery and returns what it did, in a sentence
// GitHub shows beside the delivery. ErrHookUnauthorized is a signature that
// does not match, checked before anything in the body is read.
func (s *Service) DeliverGitHub(ctx context.Context, d GitHubDelivery) (string, error) {
	if s.gh == nil {
		return "", ErrGitHubOff
	}
	if !s.gh.Verify(d.Signature, d.Body) {
		return "", ErrHookUnauthorized
	}
	d.ID = strings.TrimSpace(d.ID)
	if d.ID == "" || len(d.ID) > 100 {
		return "", ErrGitHubDeliveryInvalid
	}

	// Recorded before acting, so a redelivery racing the first is answered
	// as a duplicate rather than deploying twice; forgotten again if acting
	// fails, so GitHub's redelivery is acted on.
	n, err := s.q.RecordGitHubDelivery(ctx, dbgen.RecordGitHubDeliveryParams{ID: d.ID, Event: d.Event})
	if err != nil {
		return "", fmt.Errorf("app: record GitHub delivery: %w", err)
	}
	if n == 0 {
		return "Already handled this delivery.", nil
	}
	if _, err := s.q.PruneGitHubDeliveries(ctx); err != nil {
		s.log.Warn("prune GitHub deliveries", slog.String("error", err.Error()))
	}

	result, err := s.actOnGitHub(ctx, d)
	if err != nil {
		if fErr := s.q.ForgetGitHubDelivery(context.WithoutCancel(ctx), d.ID); fErr != nil {
			s.log.Warn("forget failed GitHub delivery", slog.String("error", fErr.Error()))
		}
		return "", err
	}
	return result, nil
}

func (s *Service) actOnGitHub(ctx context.Context, d GitHubDelivery) (string, error) {
	switch strings.ToLower(strings.TrimSpace(d.Event)) {
	case "ping":
		return "Webhook connected.", nil
	case "push":
		return s.gitHubPush(ctx, d.Body)
	case "installation":
		return s.gitHubInstallation(ctx, d.Body)
	case "installation_repositories":
		return s.gitHubInstallationRepos(ctx, d.Body)
	}
	return fmt.Sprintf("Ignored a %q event.", d.Event), nil
}

// gitHubPush deploys every app built from the pushed repository, through its
// installation, in a team still connected to it.
func (s *Service) gitHubPush(ctx context.Context, body []byte) (string, error) {
	var ev github.PushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", fmt.Errorf("app: read push payload: %w", err)
	}
	inst, repo := ev.Installation.ID, ev.Repository.ID
	if inst == 0 || repo == 0 {
		return "Ignored a push that names no installation or repository.", nil
	}

	// A renamed repository still clones from its old address, but the page
	// should say what it is called. Built from the name, like every
	// connected repository's URL, so the host is never the payload's.
	if name := ev.Repository.FullName; name != "" && (Repo{URL: gitHubCloneURL(name)}).Validate() == nil {
		if _, err := s.q.RefreshGitHubRepoURL(ctx, dbgen.RefreshGitHubRepoURLParams{
			InstallationID: inst, RepoID: repo, RepoUrl: gitHubCloneURL(name),
		}); err != nil {
			s.log.Warn("refresh renamed repository", slog.String("error", err.Error()))
		}
	}

	rows, err := s.q.ListAppsForGitHubPush(ctx, dbgen.ListAppsForGitHubPushParams{
		InstallationID: inst, RepoID: repo,
	})
	if err != nil {
		return "", fmt.Errorf("app: find apps for a push: %w", err)
	}
	if len(rows) == 0 {
		return "No app builds from " + orUnknown(ev.Repository.FullName) + ".", nil
	}

	var lines []string
	for _, row := range rows {
		a := toApp(row)
		result, err := s.gitHubPushTo(ctx, a, body)
		if err != nil {
			// One app's failure is said and logged rather than failing the
			// delivery: GitHub's redelivery would deploy every other app
			// again.
			s.log.Error("GitHub push", slog.String("team", a.OwnerID), slog.String("app", a.Name),
				slog.String("error", err.Error()))
			result = "Could not deploy: " + err.Error()
		}
		if recErr := s.q.RecordHookDelivery(ctx, dbgen.RecordHookDeliveryParams{
			AppID: a.ID, OwnerID: a.OwnerID, Result: result,
		}); recErr != nil {
			s.log.Warn("record webhook delivery", slog.String("app", a.Name),
				slog.String("error", recErr.Error()))
		}
		lines = append(lines, a.OwnerID+"/"+a.Name+": "+result)
	}
	return strings.Join(lines, "\n"), nil
}

func (s *Service) gitHubPushTo(ctx context.Context, a App, body []byte) (string, error) {
	if a.Source != SourceGit {
		return "Ignored — this app is not built from a repository.", nil
	}
	if a.GitHubPaused != "" {
		return "Ignored — builds are paused. " + GitHubPausedReason(a.GitHubPaused), nil
	}
	return s.actOnPush(ctx, a, Push{Event: "push", Body: body})
}

// gitHubInstallation records the App uninstalled, suspended or unsuspended
// on an account. Installations no team connected are no row, and nothing.
func (s *Service) gitHubInstallation(ctx context.Context, body []byte) (string, error) {
	var ev github.InstallationEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", fmt.Errorf("app: read installation payload: %w", err)
	}
	id := ev.Installation.ID
	switch ev.Action {
	case "deleted":
		s.installationGone(ctx, id)
		return fmt.Sprintf("Installation %d removed; its apps are paused.", id), nil
	case "suspend":
		s.installationSuspended(ctx, id, true)
		return fmt.Sprintf("Installation %d suspended; its apps are paused.", id), nil
	case "unsuspend":
		s.installationSuspended(ctx, id, false)
		return fmt.Sprintf("Installation %d unsuspended; its apps build again.", id), nil
	}
	return fmt.Sprintf("Ignored installation %q.", ev.Action), nil
}

// gitHubInstallationRepos records repositories taken from an installation,
// and given back, by id.
func (s *Service) gitHubInstallationRepos(ctx context.Context, body []byte) (string, error) {
	var ev github.InstallationRepositoriesEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", fmt.Errorf("app: read installation_repositories payload: %w", err)
	}
	inst := ev.Installation.ID
	ids := func(rs []github.EventRepository) []int64 {
		out := make([]int64, 0, len(rs))
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return out
	}
	var paused, resumed int64
	if removed := ids(ev.Removed); len(removed) > 0 {
		n, err := s.q.PauseGitHubRepoApps(ctx, dbgen.PauseGitHubRepoAppsParams{
			InstallationID: inst, RepoIds: removed,
		})
		if err != nil {
			return "", fmt.Errorf("app: pause apps for removed repositories: %w", err)
		}
		paused = n
		s.gh.Forget(inst)
	}
	if added := ids(ev.Added); len(added) > 0 {
		n, err := s.q.ResumeGitHubRepoApps(ctx, dbgen.ResumeGitHubRepoAppsParams{
			InstallationID: inst, RepoIds: added,
		})
		if err != nil {
			return "", fmt.Errorf("app: resume apps for added repositories: %w", err)
		}
		resumed = n
	}
	return fmt.Sprintf("%d added, %d removed; %d apps paused, %d resumed.",
		len(ev.Added), len(ev.Removed), paused, resumed), nil
}
