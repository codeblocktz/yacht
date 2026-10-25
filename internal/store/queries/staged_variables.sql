-- Variable edits waiting for a deploy, and a project's shared variables.
-- Every query filters by owner_id, for the reason apps.sql gives.

-- name: UpsertStagedVariable :one
INSERT INTO staged_variables (owner_id, app_id, key, removed, value, sealed, secret)
VALUES (@owner_id, @app_id, @key, @removed, @value, @sealed, @secret)
ON CONFLICT (app_id, key) DO UPDATE
SET removed    = excluded.removed,
    value      = excluded.value,
    sealed     = excluded.sealed,
    secret     = excluded.secret,
    updated_at = now()
WHERE staged_variables.owner_id = excluded.owner_id
RETURNING *;

-- name: GetStagedVariable :one
SELECT * FROM staged_variables
WHERE owner_id = @owner_id AND app_id = @app_id AND key = @key;

-- name: ListStagedVariablesForApp :many
SELECT * FROM staged_variables
WHERE owner_id = @owner_id AND app_id = @app_id
ORDER BY key;

-- name: DeleteStagedVariable :execrows
DELETE FROM staged_variables
WHERE owner_id = @owner_id AND app_id = @app_id AND key = @key;

-- name: DiscardStagedVariables :many
DELETE FROM staged_variables
WHERE owner_id = @owner_id AND app_id = @app_id
RETURNING key;

-- Moves every staged edit into the variables a deploy reads, in one
-- statement so a deploy can never see half of them. The app's config version
-- moves with them, as any variable edit's does; nothing is returned, and
-- nothing bumped, when there was nothing staged.
--
-- name: CommitStagedVariables :many
WITH staged AS (
    DELETE FROM staged_variables
    WHERE staged_variables.owner_id = @owner_id AND staged_variables.app_id = @app_id
    RETURNING staged_variables.*
), removed AS (
    DELETE FROM variables v
    USING staged s
    WHERE v.owner_id = @owner_id AND v.app_id = s.app_id AND v.key = s.key AND s.removed
    RETURNING v.key
), written AS (
    INSERT INTO variables (owner_id, app_id, key, value, sealed, secret)
    SELECT s.owner_id, s.app_id, s.key, s.value, s.sealed, s.secret
    FROM staged s WHERE NOT s.removed
    ON CONFLICT (app_id, key) DO UPDATE
    SET value      = excluded.value,
        sealed     = excluded.sealed,
        secret     = excluded.secret,
        updated_at = now()
    RETURNING variables.key
), bumped AS (
    UPDATE apps
    SET config_version = config_version + 1, updated_at = now()
    WHERE apps.owner_id = @owner_id AND apps.id = @app_id
      AND EXISTS (SELECT 1 FROM staged)
    RETURNING config_version
)
SELECT staged.key, staged.removed, bumped.config_version
FROM staged CROSS JOIN bumped
ORDER BY staged.key;

-- When the release an app is running went live: its newest successful
-- deployment. A rollback reuses an old release, and what it resolved its
-- secrets and references from is what was current when it ran, not when the
-- release was first made.
--
-- name: GetReleaseLiveSince :one
SELECT started_at FROM deployments
WHERE owner_id = @owner_id AND app_id = @app_id AND release_id = @release_id
  AND status = 'succeeded'
ORDER BY started_at DESC
LIMIT 1;

-- name: ListProjectVariables :many
SELECT * FROM project_variables
WHERE owner_id = @owner_id AND project_id = @project_id
ORDER BY key;

-- name: GetProjectVariable :one
SELECT * FROM project_variables
WHERE owner_id = @owner_id AND project_id = @project_id AND key = @key;

-- name: UpsertProjectVariable :one
INSERT INTO project_variables (owner_id, project_id, key, value, sealed, secret)
VALUES (@owner_id, @project_id, @key, @value, @sealed, @secret)
ON CONFLICT (project_id, key) DO UPDATE
SET value      = excluded.value,
    sealed     = excluded.sealed,
    secret     = excluded.secret,
    updated_at = now()
WHERE project_variables.owner_id = excluded.owner_id
RETURNING *;

-- name: DeleteProjectVariable :execrows
DELETE FROM project_variables
WHERE owner_id = @owner_id AND project_id = @project_id AND key = @key;
