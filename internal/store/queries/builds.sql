-- Builds. Owner-scoped in the row rather than through the deployment, because
-- the log is readable output and the scoping has to be checkable in the query
-- that reads it.

-- name: CreateBuild :one
INSERT INTO builds (owner_id, app_id, deployment_id, repo_url, repo_ref)
VALUES (@owner_id, @app_id, @deployment_id, @repo_url, @repo_ref)
RETURNING *;

-- name: AppendBuildLog :exec
-- Appended rather than replaced, so a build streaming its output does not have
-- to hold the whole log in memory to write any of it.
UPDATE builds
SET log = log || @chunk::text
WHERE id = @id;

-- name: FinishBuild :one
UPDATE builds
SET status      = @status,
    message     = @message,
    image       = @image,
    commit_sha  = @commit_sha,
    finished_at = now()
WHERE id = @id
RETURNING *;

-- name: GetBuildForDeployment :one
SELECT * FROM builds
WHERE owner_id = @owner_id AND deployment_id = @deployment_id;

-- name: GetBuild :one
SELECT * FROM builds
WHERE owner_id = @owner_id AND id = @id;
