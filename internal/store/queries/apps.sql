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

-- Every app, with how its most recent deployment ended.
--
-- A failed deploy leaves the previous workload running, so the app's live
-- status stays green and nothing in a list of apps says the last attempt to
-- change it did not take. That is exactly the deploy somebody needs to find,
-- and until this query existed the only way to find it was to open each app.
--
-- LATERAL rather than a window function or a join on max(started_at): one index
-- lookup per app, and it reads as what it is — the latest row for this app.
-- name: ListAppsWithLastDeploy :many
SELECT sqlc.embed(a), d.status AS last_deploy_status
FROM apps a
LEFT JOIN LATERAL (
    SELECT status FROM deployments
    WHERE app_id = a.id
    ORDER BY started_at DESC
    LIMIT 1
) d ON true
WHERE a.owner_id = $1
ORDER BY a.name;

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
    config_version = config_version + 1,
    updated_at     = now()
WHERE owner_id = @owner_id AND id = @id
  AND (image, port, cpu_request, cpu_limit, memory_request, memory_limit,
       internal, repo_url, repo_branch, repo_subdir)
      IS DISTINCT FROM
      (@image, @port, @cpu_request, @cpu_limit, @memory_request, @memory_limit,
       @internal, @repo_url, @repo_branch, @repo_subdir)
RETURNING *;

-- name: SetAppReplicas :one
UPDATE apps
SET replicas = $3, config_version = config_version + 1, updated_at = now()
WHERE owner_id = $1 AND id = $2 AND replicas IS DISTINCT FROM $3
RETURNING *;

-- name: DeleteApp :exec
DELETE FROM apps WHERE owner_id = $1 AND id = $2;

-- name: CreateDeployment :one
INSERT INTO deployments (
    owner_id, app_id, image, revision, status, release_id, trigger,
    actor_kind, actor_id
)
VALUES (
    @owner_id, @app_id, @image, @revision, @status, @release_id, @trigger,
    @actor_kind, @actor_id
)
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
SELECT d.*, a.name AS app_name, a.namespace AS app_namespace,
       a.active_release_id
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
    config_version  = config_version + 1,
    updated_at      = now()
WHERE owner_id = @owner_id AND id = @id
  AND (health_path, health_liveness)
      IS DISTINCT FROM (@health_path, @health_liveness)
RETURNING *;

-- name: SetAppNetworking :execrows
UPDATE apps
SET https_only = @https_only, cname_only = @cname_only,
    config_version = config_version + 1, updated_at = now()
WHERE owner_id = @owner_id AND name = @name
  AND (https_only, cname_only) IS DISTINCT FROM (@https_only, @cname_only);

-- name: GetDeployment :one
SELECT * FROM deployments
WHERE owner_id = @owner_id AND id = @id;

-- name: SetAppImage :one
-- The image a build produced. Separate from UpdateApp because a build sets
-- only this: the replicas and limits a person configured are not a build's to
-- overwrite, and passing them through would make every build a chance to.
UPDATE apps
SET image = $3, config_version = config_version + 1, updated_at = now()
WHERE owner_id = $1 AND id = $2 AND image IS DISTINCT FROM $3
RETURNING *;

-- name: SetAppRunAsUser :exec
-- Recorded by a build, which is the only thing that can discover it. The
-- image update owns the build operation's single config_version increment.
UPDATE apps
SET run_as_user = @run_as_user,
    updated_at = now()
WHERE owner_id = @owner_id AND id = @id
  AND run_as_user IS DISTINCT FROM @run_as_user;

-- name: IncrementGitAppConfigVersions :exec
-- Registry credentials are a live overlay for every Git-built image. They are
-- install-wide, so one settings mutation invalidates each Git app exactly
-- once without manufacturing release history.
UPDATE apps
SET config_version = config_version + 1, updated_at = now()
WHERE source = 'git';

-- name: IncrementCNAMEAppConfigVersions :exec
-- The platform CNAME target is an install-wide live overlay, but it only
-- renders into apps that explicitly opted into CNAME routing.
UPDATE apps
SET config_version = config_version + 1, updated_at = now()
WHERE cname_only;

-- Apps whose database pointer names the workload the cluster must converge to.
-- This is intentionally install-scoped: the reconciler acts from stored
-- ownership rather than from an authenticated request.
-- name: ListAppsForReconciliation :many
SELECT * FROM apps
WHERE active_release_id IS NOT NULL
   OR EXISTS (
       SELECT 1 FROM deployment_operations o
       WHERE o.app_id = apps.id AND o.owner_id = apps.owner_id
         AND o.status = 'queued' AND o.checkpoint IN ('applying', 'verifying')
   )
ORDER BY id;
