-- Every query filters by owner_id.
--
-- This is not defensive duplication of an application-level check — it is the
-- check. A handler that forgets to scope produces no rows here rather than
-- another owner's data, and sqlc makes the parameter impossible to omit
-- because the generated signature requires it.

-- name: CreateTeamRow :one
INSERT INTO teams (id, display_name, email)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE
    SET display_name = EXCLUDED.display_name,
        email        = EXCLUDED.email,
        updated_at   = now()
RETURNING *;

-- name: GetTeamRow :one
SELECT * FROM teams WHERE id = $1;

-- name: CreateApp :one
INSERT INTO apps (
    owner_id, name, namespace, image, replicas, port, source, internal,
    cpu_request, cpu_limit, memory_request, memory_limit, project_id,
    repo_url, repo_branch, repo_subdir
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
RETURNING *;

-- name: GetApp :one
SELECT * FROM apps
WHERE owner_id = $1 AND name = $2;

-- name: GetAppByID :one
SELECT * FROM apps
WHERE owner_id = $1 AND id = $2;

-- name: ListApps :many
SELECT * FROM apps
WHERE owner_id = $1
ORDER BY name;

-- name: CountApps :one
SELECT count(*) FROM apps WHERE owner_id = $1;

-- Everything about an app a person is allowed to change after creating it.
--
-- Deliberately not replicas: scaling has its own query because it has its own
-- rule about storage, and folding it in here would make every settings save a
-- chance to silently reset a scale somebody had chosen.
--
-- Nor the health probe, the networking toggles or run_as_user — each of those
-- already has a query shaped to what it means, and this one exists for the
-- fields that had no way to be changed at all.
-- name: UpdateApp :one
UPDATE apps
SET image          = @image,
    port           = @port,
    cpu_request    = @cpu_request,
    cpu_limit      = @cpu_limit,
    memory_request = @memory_request,
    memory_limit   = @memory_limit,
    internal       = @internal,
    repo_url       = @repo_url,
    repo_branch    = @repo_branch,
    repo_subdir    = @repo_subdir,
    updated_at     = now()
WHERE owner_id = @owner_id AND id = @id
RETURNING *;

-- name: SetAppReplicas :one
UPDATE apps
SET replicas = $3, updated_at = now()
WHERE owner_id = $1 AND id = $2
RETURNING *;

-- name: DeleteApp :exec
DELETE FROM apps WHERE owner_id = $1 AND id = $2;

-- name: CreateDeployment :one
INSERT INTO deployments (owner_id, app_id, image, revision, status)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: FinishDeployment :one
UPDATE deployments
SET status = $3, message = $4, finished_at = now()
WHERE owner_id = $1 AND id = $2
RETURNING *;

-- name: ListDeployments :many
SELECT * FROM deployments
WHERE owner_id = $1 AND app_id = $2
ORDER BY started_at DESC
LIMIT $3;

-- name: ListRecentDeployments :many
-- Joined to apps so the activity feed can name the workload without a second
-- round trip per row. The join is on app_id AND owner_id: joining on app_id
-- alone would be correct today and wrong the moment more than one owner exists.
SELECT d.*, a.name AS app_name, a.namespace AS app_namespace
FROM deployments d
JOIN apps a ON a.id = d.app_id AND a.owner_id = d.owner_id
WHERE d.owner_id = $1
ORDER BY d.started_at DESC
LIMIT $2;

-- name: DeployActivity :many
-- Deploys per day and outcome, for the overview chart.
--
-- Counted in the database rather than by reading rows and tallying them in Go:
-- a busy month is thousands of deployments, and none of them are wanted here
-- except as a number.
--
-- Days with no deploys are absent from this result. The caller fills them in;
-- see app.DeployActivity for why that cannot be skipped.
SELECT date_trunc('day', started_at)::timestamptz AS day,
       status,
       count(*)::bigint AS total
FROM deployments
WHERE owner_id = $1 AND started_at >= $2
GROUP BY 1, 2
ORDER BY 1;

-- name: SetAppHealth :one
UPDATE apps
SET health_path     = @health_path,
    health_liveness = @health_liveness,
    updated_at      = now()
WHERE owner_id = @owner_id AND id = @id
RETURNING *;

-- name: SetAppNetworking :execrows
UPDATE apps
SET https_only = @https_only, cname_only = @cname_only, updated_at = now()
WHERE owner_id = @owner_id AND name = @name;

-- Retires the deployments a new one replaces.
--
-- Only rows that never reached a terminal state: a finished deployment already
-- says what happened to it, and rewriting that would lose the difference
-- between one that was replaced and one that failed.
-- name: SupersedeDeployments :execrows
UPDATE deployments
SET status = 'superseded', finished_at = COALESCE(finished_at, now())
WHERE owner_id = @owner_id AND app_id = @app_id
  AND status IN ('running', 'active');

-- name: GetDeployment :one
SELECT * FROM deployments
WHERE owner_id = @owner_id AND id = @id;

-- name: SetAppImage :one
-- The image a build produced. Separate from UpdateApp because a build sets
-- only this: the replicas and limits a person configured are not a build's to
-- overwrite, and passing them through would make every build a chance to.
UPDATE apps
SET image = $3, updated_at = now()
WHERE owner_id = $1 AND id = $2
RETURNING *;

-- name: SetAppRunAsUser :exec
-- Recorded by a build, which is the only thing that can discover it.
UPDATE apps SET run_as_user = @run_as_user, updated_at = now()
WHERE owner_id = @owner_id AND id = @id;
