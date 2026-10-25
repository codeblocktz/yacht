-- name: CreateEnvironment :one
INSERT INTO environments (owner_id, project_id, slug, name, copied_from)
VALUES (@owner_id, @project_id, @slug, @name, @copied_from)
RETURNING *;

-- name: ListEnvironments :many
SELECT * FROM environments
WHERE owner_id = @owner_id AND project_id = @project_id
ORDER BY created_at, slug;

-- name: GetEnvironment :one
SELECT * FROM environments
WHERE owner_id = @owner_id AND project_id = @project_id AND slug = @slug;

-- name: DeleteEnvironment :execrows
DELETE FROM environments
WHERE owner_id = @owner_id AND project_id = @project_id AND slug = @slug;

-- name: ListAppsInEnvironment :many
SELECT * FROM apps
WHERE owner_id = @owner_id AND project_id = @project_id AND environment = @environment
ORDER BY name;

-- name: GetAppByService :one
-- The app that is a service in an environment, for a reference: ${{db.KEY}}
-- written in staging means staging's db.
SELECT * FROM apps
WHERE owner_id = @owner_id AND project_id = @project_id
  AND environment = @environment AND service = @service;

-- name: SetAppEnvironment :exec
-- Where an app made by Create belongs, written in Create's transaction.
UPDATE apps SET environment = @environment, service = @service
WHERE owner_id = @owner_id AND id = @id;

-- name: SetProjectPREnvironments :execrows
UPDATE projects SET pr_environments = @enabled, updated_at = now()
WHERE owner_id = @owner_id AND id = @id;

-- name: SetEnvironmentPR :exec
UPDATE environments SET pr_number = @pr_number
WHERE owner_id = @owner_id AND project_id = @project_id AND slug = @slug;

-- name: ProjectsForPullRequest :many
-- Every project that builds a repository in production and makes an
-- environment for its pull requests: one row a project, whichever team.
SELECT DISTINCT p.id, p.owner_id, p.slug, p.name
FROM projects p
JOIN apps a ON a.project_id = p.id
WHERE p.pr_environments
  AND a.environment = 'production'
  AND a.github_installation_id = @installation_id
  AND a.github_repo_id = @repo_id;
