-- The install's GitHub App. See migration 00037.

-- name: UpsertGitHubInstallation :exec
-- Written when a team connects one, with what GitHub said the account is.
-- The account's name is refreshed rather than kept: an organisation can be
-- renamed, and the page should say what it is called now.
INSERT INTO github_installations (id, account_login, account_type)
VALUES (@id, @account_login, @account_type)
ON CONFLICT (id) DO UPDATE
SET account_login = excluded.account_login,
    account_type  = excluded.account_type,
    updated_at    = now();

-- name: ConnectTeamGitHubInstallation :exec
-- Connecting again is how somebody else on the team takes it over, so the
-- record of who connected it follows the latest proof.
INSERT INTO team_github_installations (
    owner_id, installation_id, connected_by_email, connected_by_login
)
VALUES (@owner_id, @installation_id, @connected_by_email, @connected_by_login)
ON CONFLICT (owner_id, installation_id) DO UPDATE
SET connected_by_email = excluded.connected_by_email,
    connected_by_login = excluded.connected_by_login,
    connected_at       = now();

-- name: ListTeamGitHubInstallations :many
SELECT t.installation_id, t.connected_by_email, t.connected_by_login, t.connected_at,
       i.account_login, i.account_type, i.suspended_at, i.removed_at
FROM team_github_installations t
JOIN github_installations i ON i.id = t.installation_id
WHERE t.owner_id = @owner_id
ORDER BY lower(i.account_login), t.installation_id;

-- name: GetTeamGitHubInstallation :one
-- The check every use of an installation goes through: a team reads the
-- repositories of an installation only if it is connected to it.
SELECT t.installation_id, t.connected_by_email, t.connected_by_login, t.connected_at,
       i.account_login, i.account_type, i.suspended_at, i.removed_at
FROM team_github_installations t
JOIN github_installations i ON i.id = t.installation_id
WHERE t.owner_id = @owner_id AND t.installation_id = @installation_id;

-- name: DisconnectTeamGitHubInstallation :execrows
DELETE FROM team_github_installations
WHERE owner_id = @owner_id AND installation_id = @installation_id;

-- name: PauseTeamGitHubApps :execrows
-- A team letting go of an installation stops its apps building from it.
-- Whatever paused them already stays the reason.
UPDATE apps
SET github_paused = 'disconnected', updated_at = now()
WHERE owner_id = @owner_id AND github_installation_id = @installation_id::bigint
  AND github_paused = '';

-- name: ResumeTeamGitHubApps :execrows
-- And connecting it again resumes what disconnecting paused, nothing else.
UPDATE apps
SET github_paused = '', updated_at = now()
WHERE owner_id = @owner_id AND github_installation_id = @installation_id::bigint
  AND github_paused = 'disconnected';

-- name: SetGitHubInstallationSuspended :execrows
UPDATE github_installations
SET suspended_at = CASE WHEN @suspended::boolean THEN now() ELSE NULL END,
    updated_at   = now()
WHERE id = @id;

-- name: MarkGitHubInstallationRemoved :execrows
UPDATE github_installations
SET removed_at = now(), updated_at = now()
WHERE id = @id AND removed_at IS NULL;

-- name: PauseGitHubInstallationApps :execrows
-- Across every team: an installation suspended or removed on GitHub is so for
-- all of them. Removal is final and wins over any other reason.
UPDATE apps
SET github_paused = @reason, updated_at = now()
WHERE github_installation_id = @installation_id::bigint
  AND github_paused NOT IN (@reason, 'installation_removed');

-- name: ResumeGitHubInstallationApps :execrows
UPDATE apps
SET github_paused = '', updated_at = now()
WHERE github_installation_id = @installation_id::bigint AND github_paused = @reason;

-- name: PauseGitHubRepoApps :execrows
-- Repositories the installation no longer reaches, by id.
UPDATE apps
SET github_paused = 'repo_removed', updated_at = now()
WHERE github_installation_id = @installation_id::bigint
  AND github_repo_id = ANY(@repo_ids::bigint[])
  AND github_paused = '';

-- name: ResumeGitHubRepoApps :execrows
UPDATE apps
SET github_paused = '', updated_at = now()
WHERE github_installation_id = @installation_id::bigint
  AND github_repo_id = ANY(@repo_ids::bigint[])
  AND github_paused = 'repo_removed';

-- name: SetAppGitHubSource :exec
-- Choosing a repository, or pasting a URL in its place, which clears it.
-- Either way builds resume: whatever paused them was about the repository
-- that was there before.
UPDATE apps
SET github_installation_id = sqlc.narg(installation_id),
    github_repo_id         = sqlc.narg(repo_id),
    github_paused          = '',
    updated_at             = now()
WHERE owner_id = @owner_id AND id = @id;

-- name: ListAppsForGitHubPush :many
-- Every app built from one repository through one installation, across teams
-- — but only teams still connected to it, so an app left behind by a team
-- that disconnected deploys nothing on a push.
SELECT a.*
FROM apps a
JOIN team_github_installations t
  ON t.owner_id = a.owner_id AND t.installation_id = a.github_installation_id
WHERE a.github_installation_id = @installation_id::bigint
  AND a.github_repo_id = @repo_id::bigint
ORDER BY a.owner_id, a.name;

-- name: RefreshGitHubRepoURL :execrows
-- A push carries the repository's current name. A renamed repository still
-- clones from its old address, but the page should show the new one.
UPDATE apps
SET repo_url = @repo_url
WHERE github_installation_id = @installation_id::bigint
  AND github_repo_id = @repo_id::bigint
  AND repo_url <> @repo_url;

-- name: RecordGitHubDelivery :execrows
-- Zero rows is a delivery already acted on.
INSERT INTO github_deliveries (id, event)
VALUES (@id, @event)
ON CONFLICT (id) DO NOTHING;

-- name: ForgetGitHubDelivery :exec
-- A delivery that failed half way is forgotten, so GitHub's redelivery of it
-- is acted on rather than answered as a duplicate.
DELETE FROM github_deliveries WHERE id = @id;

-- name: PruneGitHubDeliveries :execrows
DELETE FROM github_deliveries WHERE received_at < now() - interval '7 days';
