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
    cpu_request, cpu_limit, memory_request, memory_limit, project_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
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

-- name: UpdateApp :one
UPDATE apps
SET image          = $3,
    replicas       = $4,
    port           = $5,
    cpu_request    = $6,
    cpu_limit      = $7,
    memory_request = $8,
    memory_limit   = $9,
    updated_at     = now()
WHERE owner_id = $1 AND id = $2
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
