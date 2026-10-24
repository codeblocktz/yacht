package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/github"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// The install's GitHub App, as teams use it.
//
// A team connects an installation of the App — on a GitHub account or an
// organisation, reaching the repositories somebody chose there — and then
// picks repositories from it instead of pasting URLs. Builds clone them with
// a token scoped to the one repository, so private ones work; a push to one
// arrives at the App's single webhook and deploys every app built from it,
// through the same admission, fenced build and rollout check a push to an
// app's own webhook takes.
//
// Which team may use which installation is the one decision here that is
// Yacht's rather than GitHub's, and it is only ever made from GitHub's answer
// to the person's own authorization: an installation id on its own proves
// nothing.

// Why builds from a connected repository stopped. See migration 00037.
const (
	GitHubPausedRepoRemoved  = "repo_removed"
	GitHubPausedSuspended    = "installation_suspended"
	GitHubPausedUninstalled  = "installation_removed"
	GitHubPausedDisconnected = "disconnected"
)

// gitHubCloneURLPrefix is the only host a connected repository is cloned
// from, and so the only one its token is ever sent to.
const gitHubCloneURLPrefix = "https://github.com/"

// GitHub refusals the web layer turns into sentences.
var (
	// ErrGitHubOff is an install with no GitHub App configured.
	ErrGitHubOff = errors.New("app: this install has no GitHub App")

	// ErrGitHubNotYours is an installation the person who came back from
	// GitHub cannot reach — the installation id they arrived with was not
	// among GitHub's answer to their own authorization.
	ErrGitHubNotYours = errors.New(
		"app: GitHub does not list that installation among yours — sign in to GitHub " +
			"as somebody who can manage it, and connect again")

	// ErrGitHubNoInstallation is a person GitHub lists no installation of
	// the App for, who needs to install it first.
	ErrGitHubNoInstallation = errors.New(
		"app: the GitHub App is not installed on any account you can reach — install it first")

	// ErrGitHubNotConnected is an installation this team has not connected,
	// or has since disconnected. Said the same whether or not another team
	// has: which installations other teams use is theirs.
	ErrGitHubNotConnected = errors.New("app: this team has not connected that GitHub installation")

	// ErrGitHubStateMismatch is a GitHub link made for another team or
	// person than the session it came back with.
	ErrGitHubStateMismatch = errors.New(
		"app: that GitHub link was made for another team or person — start again from the Team page")
)

// GitHubConnection is an installation a team has connected.
type GitHubConnection struct {
	InstallationID int64

	// Account is the GitHub user or organisation it is installed on, and
	// AccountType GitHub's "User" or "Organization".
	Account     string
	AccountType string

	// ConnectedBy is who connected it here, and ConnectedAs the GitHub
	// account that proved access.
	ConnectedBy string
	ConnectedAs string
	ConnectedAt time.Time

	// Suspended and Removed are what GitHub last said of it.
	Suspended bool
	Removed   bool
}

// Usable reports whether builds and pickers can use it.
func (c GitHubConnection) Usable() bool { return !c.Suspended && !c.Removed }

// GitHubChoice is one installation GitHub listed as the person's, offered
// for them to pick when there are several. Token carries the proof to the
// form that picks it.
type GitHubChoice struct {
	InstallationID int64
	Account        string
	AccountType    string
	Token          string
}

// GitHubConnect is what a return from GitHub came to: one installation
// connected, or several to choose from.
type GitHubConnect struct {
	Connected GitHubConnection
	Choices   []GitHubChoice
}

// GitHubRepo is a repository a team can build from through one of its
// installations.
type GitHubRepo struct {
	Ref      GitHubRef
	FullName string
	Private  bool
	Account  string
}

// GitHubEnabled reports whether this install has a GitHub App.
func (s *Service) GitHubEnabled() bool { return s.gh != nil }

// GitHubAppSlug is the App's name in its URL, for a page to link to it.
func (s *Service) GitHubAppSlug() string {
	if s.gh == nil {
		return ""
	}
	return s.gh.Slug()
}

// GitHubInstallURL is where a person goes to install the App, or change
// which repositories an installation reaches, for team ownerID. user is who
// they are here; the link works for nobody else.
func (s *Service) GitHubInstallURL(ownerID, user string) (string, error) {
	if s.gh == nil {
		return "", ErrGitHubOff
	}
	return s.gh.InstallURL(s.gh.SignState(ownerID, user)), nil
}

// GitHubAuthorizeURL is the same for an account that already has the App:
// the person authorizes, and GitHub lists what they can reach.
func (s *Service) GitHubAuthorizeURL(ownerID, user string) (string, error) {
	if s.gh == nil {
		return "", ErrGitHubOff
	}
	return s.gh.AuthorizeURL(s.gh.SignState(ownerID, user)), nil
}

// GitHubConnections lists the installations a team has connected.
func (s *Service) GitHubConnections(ctx context.Context, ownerID string) ([]GitHubConnection, error) {
	rows, err := s.q.ListTeamGitHubInstallations(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("app: list GitHub installations: %w", err)
	}
	out := make([]GitHubConnection, 0, len(rows))
	for _, r := range rows {
		out = append(out, GitHubConnection{
			InstallationID: r.InstallationID, Account: r.AccountLogin, AccountType: r.AccountType,
			ConnectedBy: r.ConnectedByEmail, ConnectedAs: r.ConnectedByLogin, ConnectedAt: r.ConnectedAt,
			Suspended: r.SuspendedAt.Valid, Removed: r.RemovedAt.Valid,
		})
	}
	return out, nil
}

// CompleteGitHubConnect is the return from GitHub: state as the install link
// carried it, the code GitHub issued to the person, and the installation id
// GitHub named, zero when they only authorized.
//
// ownerID and user are the session's, and must be the ones the state was
// signed for. The installation is connected only if GitHub's answer to the
// code lists it; with none named, the one GitHub lists is connected, or the
// several are offered to choose from.
func (s *Service) CompleteGitHubConnect(
	ctx context.Context, ownerID, user, email, state, code string, installationID int64,
) (GitHubConnect, error) {
	if s.gh == nil {
		return GitHubConnect{}, ErrGitHubOff
	}
	st, err := s.gh.OpenState(state, github.StateLink)
	if err != nil {
		return GitHubConnect{}, err
	}
	if st.Team != ownerID || st.User != user {
		return GitHubConnect{}, ErrGitHubStateMismatch
	}

	login, reachable, err := s.gh.UserInstallations(ctx, code)
	if err != nil {
		return GitHubConnect{}, err
	}

	if installationID != 0 {
		for _, in := range reachable {
			if in.ID == installationID {
				c, err := s.connectGitHub(ctx, ownerID, email, login, in)
				return GitHubConnect{Connected: c}, err
			}
		}
		s.log.Warn("GitHub installation not among the person's own",
			slog.String("team", ownerID), slog.Int64("installation", installationID),
			slog.String("github_login", login))
		return GitHubConnect{}, ErrGitHubNotYours
	}

	switch len(reachable) {
	case 0:
		return GitHubConnect{}, ErrGitHubNoInstallation
	case 1:
		c, err := s.connectGitHub(ctx, ownerID, email, login, reachable[0])
		return GitHubConnect{Connected: c}, err
	}
	var out GitHubConnect
	for _, in := range reachable {
		out.Choices = append(out.Choices, GitHubChoice{
			InstallationID: in.ID, Account: in.Account.Login, AccountType: in.Account.Type,
			Token: s.gh.SignChoice(ownerID, user, in.ID, login),
		})
	}
	return out, nil
}

// ChooseGitHubInstallation connects the installation a choice names: one
// GitHub listed as the person's a moment ago, for this team and person.
func (s *Service) ChooseGitHubInstallation(
	ctx context.Context, ownerID, user, email, choice string,
) (GitHubConnection, error) {
	if s.gh == nil {
		return GitHubConnection{}, ErrGitHubOff
	}
	st, err := s.gh.OpenState(choice, github.StateChoice)
	if err != nil {
		return GitHubConnection{}, err
	}
	if st.Team != ownerID || st.User != user {
		return GitHubConnection{}, ErrGitHubStateMismatch
	}
	// Asked as the App, for the account it is on: the choice carries only
	// the id that was proven.
	in, err := s.gh.Installation(ctx, st.Installation)
	if err != nil {
		return GitHubConnection{}, err
	}
	return s.connectGitHub(ctx, ownerID, email, st.Login, in)
}

// connectGitHub records a proven installation for a team, and resumes what
// the team's disconnecting it had paused.
func (s *Service) connectGitHub(
	ctx context.Context, ownerID, email, login string, in github.Installation,
) (GitHubConnection, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return GitHubConnection{}, fmt.Errorf("app: begin GitHub connect: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)

	if err := q.UpsertGitHubInstallation(ctx, dbgen.UpsertGitHubInstallationParams{
		ID: in.ID, AccountLogin: in.Account.Login, AccountType: in.Account.Type,
	}); err != nil {
		return GitHubConnection{}, fmt.Errorf("app: record GitHub installation: %w", err)
	}
	if _, err := q.SetGitHubInstallationSuspended(ctx, dbgen.SetGitHubInstallationSuspendedParams{
		ID: in.ID, Suspended: in.SuspendedAt != nil,
	}); err != nil {
		return GitHubConnection{}, fmt.Errorf("app: record GitHub installation: %w", err)
	}
	if err := q.ConnectTeamGitHubInstallation(ctx, dbgen.ConnectTeamGitHubInstallationParams{
		OwnerID: ownerID, InstallationID: in.ID, ConnectedByEmail: email, ConnectedByLogin: login,
	}); err != nil {
		return GitHubConnection{}, fmt.Errorf("app: connect GitHub installation: %w", err)
	}
	if _, err := q.ResumeTeamGitHubApps(ctx, dbgen.ResumeTeamGitHubAppsParams{
		OwnerID: ownerID, InstallationID: in.ID,
	}); err != nil {
		return GitHubConnection{}, fmt.Errorf("app: resume apps: %w", err)
	}
	row, err := q.GetTeamGitHubInstallation(ctx, dbgen.GetTeamGitHubInstallationParams{
		OwnerID: ownerID, InstallationID: in.ID,
	})
	if err != nil {
		return GitHubConnection{}, fmt.Errorf("app: read GitHub installation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return GitHubConnection{}, fmt.Errorf("app: commit GitHub connect: %w", err)
	}
	s.log.Info("GitHub installation connected", slog.String("team", ownerID),
		slog.Int64("installation", in.ID), slog.String("account", in.Account.Login),
		slog.String("by", email), slog.String("github_login", login))
	return GitHubConnection{
		InstallationID: row.InstallationID, Account: row.AccountLogin, AccountType: row.AccountType,
		ConnectedBy: row.ConnectedByEmail, ConnectedAs: row.ConnectedByLogin, ConnectedAt: row.ConnectedAt,
		Suspended: row.SuspendedAt.Valid, Removed: row.RemovedAt.Valid,
	}, nil
}

// DisconnectGitHub lets go of an installation. The team's apps built through
// it are paused, not deleted: they keep running on their last image, and
// connecting it again resumes them. The installation itself stays on GitHub,
// where only its account can remove it.
func (s *Service) DisconnectGitHub(ctx context.Context, ownerID string, installationID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("app: begin GitHub disconnect: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	n, err := q.DisconnectTeamGitHubInstallation(ctx, dbgen.DisconnectTeamGitHubInstallationParams{
		OwnerID: ownerID, InstallationID: installationID,
	})
	if err != nil {
		return fmt.Errorf("app: disconnect GitHub installation: %w", err)
	}
	if n == 0 {
		return ErrGitHubNotConnected
	}
	if _, err := q.PauseTeamGitHubApps(ctx, dbgen.PauseTeamGitHubAppsParams{
		OwnerID: ownerID, InstallationID: installationID,
	}); err != nil {
		return fmt.Errorf("app: pause apps: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app: commit GitHub disconnect: %w", err)
	}
	s.log.Info("GitHub installation disconnected", slog.String("team", ownerID),
		slog.Int64("installation", installationID))
	return nil
}

// connection is the check every use of an installation goes through.
func (s *Service) connection(ctx context.Context, ownerID string, installationID int64) error {
	if s.gh == nil {
		return ErrGitHubOff
	}
	row, err := s.q.GetTeamGitHubInstallation(ctx, dbgen.GetTeamGitHubInstallationParams{
		OwnerID: ownerID, InstallationID: installationID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrGitHubNotConnected
	}
	if err != nil {
		return fmt.Errorf("app: read GitHub installation: %w", err)
	}
	switch {
	case row.RemovedAt.Valid:
		return github.ErrInstallationGone
	case row.SuspendedAt.Valid:
		return github.ErrInstallationSuspended
	}
	return nil
}

// GitHubRepos lists every repository the team's installations reach, by
// name. An installation GitHub will not answer for is skipped and said in
// the second result, so one suspended account does not empty the picker.
func (s *Service) GitHubRepos(ctx context.Context, ownerID string) ([]GitHubRepo, []string, error) {
	conns, err := s.GitHubConnections(ctx, ownerID)
	if err != nil {
		return nil, nil, err
	}
	var repos []GitHubRepo
	var problems []string
	for _, c := range conns {
		if !c.Usable() {
			continue
		}
		list, err := s.gh.Repositories(ctx, c.InstallationID)
		if err != nil {
			s.log.Warn("list GitHub repositories", slog.Int64("installation", c.InstallationID),
				slog.String("error", err.Error()))
			problems = append(problems, fmt.Sprintf("%s: %s", c.Account, gitHubSentence(err)))
			continue
		}
		for _, r := range list {
			repos = append(repos, GitHubRepo{
				Ref:      GitHubRef{InstallationID: c.InstallationID, RepoID: r.ID},
				FullName: r.FullName, Private: r.Private, Account: c.Account,
			})
		}
	}
	slices.SortFunc(repos, func(a, b GitHubRepo) int {
		return cmp.Compare(strings.ToLower(a.FullName), strings.ToLower(b.FullName))
	})
	return repos, problems, nil
}

// GitHubBranches lists a connected repository's branches, filtered like
// Branches and read with the repository's own token, so a private one
// answers too.
func (s *Service) GitHubBranches(
	ctx context.Context, ownerID string, ref GitHubRef, query string,
) ([]string, error) {
	if err := s.connection(ctx, ownerID, ref.InstallationID); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, branchTimeout)
	defer cancel()
	all, err := s.gh.Branches(ctx, ref.InstallationID, ref.RepoID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrRepoUnreachable, gitHubSentence(err))
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	var names []string
	for _, n := range all {
		if needle == "" || strings.Contains(strings.ToLower(n), needle) {
			names = append(names, n)
		}
	}
	sortBranches(names)
	if len(names) > maxBranches {
		names = names[:maxBranches]
	}
	return names, nil
}

// GitHubDirectories is Directories for a connected repository, read with
// its own token.
func (s *Service) GitHubDirectories(
	ctx context.Context, ownerID string, ref GitHubRef, path string,
) ([]string, error) {
	if err := s.connection(ctx, ownerID, ref.InstallationID); err != nil {
		return nil, err
	}
	path = strings.Trim(strings.TrimSpace(path), "/")
	if strings.Contains(path, "..") {
		return nil, fmt.Errorf("app: %q is not a path inside the repository", path)
	}
	ctx, cancel := context.WithTimeout(ctx, branchTimeout)
	defer cancel()
	entries, err := s.gh.Contents(ctx, ref.InstallationID, ref.RepoID, path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrRepoUnreachable, gitHubSentence(err))
	}
	var dirs []string
	for _, e := range entries {
		if e.Type == "dir" && !strings.HasPrefix(e.Name, ".") {
			dirs = append(dirs, e.Name)
		}
	}
	slices.Sort(dirs)
	if len(dirs) > maxEntries {
		dirs = dirs[:maxEntries]
	}
	return dirs, nil
}

// resolveGitHubRepo turns a chosen repository into the one to store: the
// team must be connected to the installation, and the installation must
// reach the repository — which GitHub answers by scoping a token to it.
func (s *Service) resolveGitHubRepo(ctx context.Context, ownerID string, r Repo) (Repo, error) {
	if err := s.connection(ctx, ownerID, r.GitHub.InstallationID); err != nil {
		return Repo{}, err
	}
	repo, err := s.gh.Repository(ctx, r.GitHub.InstallationID, r.GitHub.RepoID)
	if err != nil {
		return Repo{}, fmt.Errorf("app: %s", gitHubSentence(err))
	}
	r.URL = gitHubCloneURL(repo.FullName)
	if r.Branch == "" {
		r.Branch = repo.DefaultBranch
	}
	return r, nil
}

// gitHubCloneURL is the address a connected repository is cloned from and
// shown as. Built from the name rather than taken from GitHub's clone_url,
// so the host the token is sent to is this constant and never a value read
// off the wire.
func gitHubCloneURL(fullName string) string {
	return gitHubCloneURLPrefix + fullName + ".git"
}

// gitHubSentence says what went wrong with GitHub, for a page.
func gitHubSentence(err error) string {
	switch {
	case errors.Is(err, github.ErrInstallationGone):
		return "the GitHub App is no longer installed on that account"
	case errors.Is(err, github.ErrInstallationSuspended):
		return "the GitHub App's installation is suspended"
	case errors.Is(err, github.ErrRepoNotAccessible):
		return "the GitHub App cannot reach that repository — choose it in the installation's settings on GitHub"
	case errors.Is(err, ErrGitHubNotConnected):
		return "this team has not connected that GitHub installation"
	}
	return strings.TrimPrefix(err.Error(), "github: ")
}

// GitHubPausedReason says why builds from an app's repository have stopped.
func GitHubPausedReason(reason string) string {
	switch reason {
	case GitHubPausedRepoRemoved:
		return "The GitHub App no longer reaches this repository. Add it to the installation on GitHub, or choose another."
	case GitHubPausedSuspended:
		return "The GitHub App's installation is suspended on GitHub."
	case GitHubPausedUninstalled:
		return "The GitHub App was uninstalled from this repository's account. Install it again and choose the repository."
	case GitHubPausedDisconnected:
		return "This team disconnected the GitHub installation this repository came through. Connect it again on the Team page."
	}
	return ""
}

// ErrGitHubPaused is a build refused because its repository's access went
// away. It carries the reason.
type ErrGitHubPaused struct{ Reason string }

func (e ErrGitHubPaused) Error() string {
	return "app: builds from this repository are paused — " + GitHubPausedReason(e.Reason)
}

// gitHubCloneToken is the token a build of a connected repository clones
// with: scoped to the one repository, read-only, never stored. An app whose
// access went away is refused before anything is minted; one whose access
// GitHub now refuses is paused, so the next push does not try again.
func (s *Service) gitHubCloneToken(ctx context.Context, a App) (string, error) {
	if !a.Repo.GitHub.Set() {
		return "", nil
	}
	if a.GitHubPaused != "" {
		return "", ErrGitHubPaused{Reason: a.GitHubPaused}
	}
	if s.gh == nil {
		return "", errors.New("app: this app's repository is reached through the GitHub App, " +
			"and this install no longer has one configured")
	}
	// The token goes to whatever host the URL names, so the URL has to be
	// GitHub's. Every write of a connected repository's URL makes it so;
	// this is the backstop.
	if !strings.HasPrefix(a.Repo.URL, gitHubCloneURLPrefix) {
		return "", errors.New("app: a repository reached through the GitHub App must be cloned from github.com")
	}
	ref := a.Repo.GitHub
	if err := s.connection(ctx, a.OwnerID, ref.InstallationID); err != nil {
		return "", fmt.Errorf("app: %s", gitHubSentence(err))
	}
	tok, err := s.gh.RepoToken(ctx, ref.InstallationID, ref.RepoID)
	if err == nil {
		return tok.Value, nil
	}
	// What GitHub says about access is recorded as the webhook would have.
	switch {
	case errors.Is(err, github.ErrRepoNotAccessible):
		s.pauseRepos(ctx, ref.InstallationID, []int64{ref.RepoID})
	case errors.Is(err, github.ErrInstallationGone):
		s.installationGone(ctx, ref.InstallationID)
	case errors.Is(err, github.ErrInstallationSuspended):
		s.installationSuspended(ctx, ref.InstallationID, true)
	}
	return "", fmt.Errorf("app: %s", gitHubSentence(err))
}

func (s *Service) pauseRepos(ctx context.Context, installationID int64, ids []int64) {
	if _, err := s.q.PauseGitHubRepoApps(ctx, dbgen.PauseGitHubRepoAppsParams{
		InstallationID: installationID, RepoIds: ids,
	}); err != nil {
		s.log.Warn("pause apps for removed repositories", slog.String("error", err.Error()))
	}
	if s.gh != nil {
		s.gh.Forget(installationID)
	}
}

func (s *Service) installationGone(ctx context.Context, installationID int64) {
	if _, err := s.q.MarkGitHubInstallationRemoved(ctx, installationID); err != nil {
		s.log.Warn("record GitHub installation removed", slog.String("error", err.Error()))
	}
	if _, err := s.q.PauseGitHubInstallationApps(ctx, dbgen.PauseGitHubInstallationAppsParams{
		InstallationID: installationID, Reason: GitHubPausedUninstalled,
	}); err != nil {
		s.log.Warn("pause apps for a removed installation", slog.String("error", err.Error()))
	}
	if s.gh != nil {
		s.gh.Forget(installationID)
	}
}

func (s *Service) installationSuspended(ctx context.Context, installationID int64, suspended bool) {
	if _, err := s.q.SetGitHubInstallationSuspended(ctx, dbgen.SetGitHubInstallationSuspendedParams{
		ID: installationID, Suspended: suspended,
	}); err != nil {
		s.log.Warn("record GitHub installation suspension", slog.String("error", err.Error()))
	}
	var err error
	if suspended {
		_, err = s.q.PauseGitHubInstallationApps(ctx, dbgen.PauseGitHubInstallationAppsParams{
			InstallationID: installationID, Reason: GitHubPausedSuspended,
		})
	} else {
		_, err = s.q.ResumeGitHubInstallationApps(ctx, dbgen.ResumeGitHubInstallationAppsParams{
			InstallationID: installationID, Reason: GitHubPausedSuspended,
		})
	}
	if err != nil {
		s.log.Warn("pause or resume apps for a suspension", slog.String("error", err.Error()))
	}
	if s.gh != nil {
		s.gh.Forget(installationID)
	}
}
