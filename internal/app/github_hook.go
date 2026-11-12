package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"

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
		// A push some of whose apps could not be checked is kept, and sent
		// again it reaches only those: the others took it the first time.
		if strings.EqualFold(strings.TrimSpace(d.Event), "push") {
			return s.gitHubPush(ctx, d.Body, true)
		}
		return "Already handled this delivery.", nil
	}
	if _, err := s.q.PruneGitHubDeliveries(ctx); err != nil {
		s.log.Warn("prune GitHub deliveries", slog.String("error", err.Error()))
	}

	result, err := s.actOnGitHub(ctx, d)
	if errors.Is(err, errPushUnchecked) {
		return "", err
	}
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
		return s.gitHubPush(ctx, d.Body, false)
	case "pull_request":
		return s.gitHubPullRequest(ctx, d.Body)
	case "installation":
		return s.gitHubInstallation(ctx, d.Body)
	case "installation_repositories":
		return s.gitHubInstallationRepos(ctx, d.Body)
	}
	return fmt.Sprintf("Ignored a %q event.", d.Event), nil
}

// gitHubPush deploys every app built from the pushed repository, through its
// installation, in a team still connected to it.
func (s *Service) gitHubPush(ctx context.Context, body []byte, again bool) (string, error) {
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
	if again {
		if rows, err = s.uncheckedOnly(ctx, rows); err != nil {
			return "", err
		}
		if len(rows) == 0 {
			return "Already handled this delivery.", nil
		}
	}

	var lines []string
	unchecked := false
	for _, row := range rows {
		a := toApp(row)
		result, err := s.gitHubPushTo(ctx, a, body)
		if errors.Is(err, ErrAdmissionUnavailable) {
			// The wrapper could not say whether this app may deploy. Said on
			// the app, and the delivery fails, to be sent again: kept as
			// handled, so sent again it reaches only the apps that say so.
			s.log.Warn("GitHub push: admission could not be checked",
				slog.String("team", a.OwnerID), slog.String("app", a.Name))
			result, err, unchecked = uncheckedPush, nil, true
		}
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
	if unchecked {
		return "", errPushUnchecked
	}
	return strings.Join(lines, "\n"), nil
}

// uncheckedPush is an app's last delivery when whether it may deploy could
// not be checked.
const uncheckedPush = "Could not check this deploy right now; push again or redeploy."

// errPushUnchecked fails a GitHub delivery some of whose apps could not be
// checked, so it shows as failed and can be sent again. Unlike any other
// failure the delivery stays recorded: see DeliverGitHub.
var errPushUnchecked = errors.New("app: whether some apps may deploy could not be checked — send the delivery again")

func (s *Service) gitHubPushTo(ctx context.Context, a App, body []byte) (string, error) {
	if a.Source != SourceGit {
		return "Ignored — this app is not built from a repository.", nil
	}
	if a.GitHubPaused != "" {
		return "Ignored — builds are paused. " + GitHubPausedReason(a.GitHubPaused), nil
	}
	return s.actOnPush(ctx, a, Push{Event: "push", Body: body, via: AdmitGitHubPush})
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

// gitHubPullRequest makes a project's environment for a pull request when it
// opens and takes it down when it closes, as Railway's PR Environments do.
// The environment is production copied, with every service built from the
// pull request's repository building its branch instead; pushes to that
// branch then deploy it the way any push deploys an app on its branch.
func (s *Service) gitHubPullRequest(ctx context.Context, body []byte) (string, error) {
	var ev github.PullRequestEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", fmt.Errorf("app: read pull request payload: %w", err)
	}
	inst, repo, number := ev.Installation.ID, ev.Repository.ID, ev.Number
	if inst == 0 || repo == 0 || number == 0 {
		return "Ignored a pull request that names no installation, repository or number.", nil
	}
	// A pull request from a fork is not built. Anybody can open one, and
	// its code would run with the project's variables — its secrets among
	// them — in an environment that looks like the project's own.
	if ev.PullRequest.Head.Repo.ID != repo {
		return fmt.Sprintf("Ignored pull request #%d: it comes from a fork, and a fork's code is not run with this project's variables.", number), nil
	}
	projects, err := s.q.ProjectsForPullRequest(ctx, dbgen.ProjectsForPullRequestParams{
		InstallationID: &inst, RepoID: &repo,
	})
	if err != nil {
		return "", fmt.Errorf("app: find projects for a pull request: %w", err)
	}
	if len(projects) == 0 {
		return "No project makes environments for " + orUnknown(ev.Repository.FullName) + "'s pull requests.", nil
	}
	slug := prEnvironmentSlug(number)
	branch := ev.PullRequest.Head.Ref
	builds := func(a App) string {
		if a.Source == SourceGit && a.Repo.GitHub.InstallationID == inst && a.Repo.GitHub.RepoID == repo {
			return branch
		}
		return ""
	}

	var lines []string
	for _, row := range projects {
		p := Project{ID: row.ID, OwnerID: row.OwnerID, Slug: row.Slug, Name: row.Name}
		has, err := s.HasEnvironment(ctx, p.OwnerID, p, slug)
		if err != nil {
			lines = append(lines, p.OwnerID+"/"+p.Slug+": "+err.Error())
			continue
		}
		switch ev.Action {
		case "opened", "reopened", "ready_for_review":
			if has {
				lines = append(lines, p.OwnerID+"/"+p.Slug+": "+slug+" already exists")
				continue
			}
			env, copies, err := s.createEnvironment(ctx, p.OwnerID, p.Slug, slug, DefaultEnvironment,
				envOptions{pr: number, branch: builds})
			if err != nil {
				if refusal, ok := isRefusal(err); ok {
					lines = append(lines, p.OwnerID+"/"+p.Slug+": "+refusedDelivery(refusal))
					continue
				}
				s.log.Error("PR environment", slog.String("project", p.Slug), slog.String("error", err.Error()))
				lines = append(lines, p.OwnerID+"/"+p.Slug+": could not make "+slug+": "+err.Error())
				continue
			}
			lines = append(lines, fmt.Sprintf("%s/%s: %s made from production, %d services, building %s",
				p.OwnerID, p.Slug, env.Slug, len(copies), branch))
		case "closed":
			if !has {
				continue
			}
			if err := s.DeleteEnvironment(ctx, p.OwnerID, p.Slug, slug); err != nil {
				s.log.Error("PR environment", slog.String("project", p.Slug), slog.String("error", err.Error()))
				lines = append(lines, p.OwnerID+"/"+p.Slug+": could not take down "+slug+": "+err.Error())
				continue
			}
			lines = append(lines, p.OwnerID+"/"+p.Slug+": "+slug+" taken down")
		}
	}
	if len(lines) == 0 {
		return fmt.Sprintf("Nothing to do for pull request #%d %s.", number, ev.Action), nil
	}
	return strings.Join(lines, "\n"), nil
}

// uncheckedOnly is the apps a push reaches whose last delivery says it
// could not be checked: what a push sent again acts on.
func (s *Service) uncheckedOnly(ctx context.Context, rows []dbgen.App) ([]dbgen.App, error) {
	var out []dbgen.App
	for _, row := range rows {
		hook, err := s.q.GetAppHook(ctx, dbgen.GetAppHookParams{OwnerID: row.OwnerID, AppID: row.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("app: read webhook: %w", err)
		}
		if hook.LastDelivery == uncheckedPush {
			out = append(out, row)
		}
	}
	return out, nil
}
