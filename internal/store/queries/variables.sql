-- Environment variables, one row each so a secret can be sealed while its
-- neighbour stays readable.

-- Upsert rather than insert: setting a variable that already exists is what a
-- person means by editing one, and a separate update path would need the
-- caller to know which case they are in.
-- name: UpsertVariable :one
INSERT INTO variables (owner_id, app_id, key, value, sealed, secret)
VALUES (@owner_id, @app_id, @key, @value, @sealed, @secret)
ON CONFLICT (app_id, key) DO UPDATE
SET value      = excluded.value,
    sealed     = excluded.sealed,
    secret     = excluded.secret,
    updated_at = now()
RETURNING *;

-- name: ListVariablesForApp :many
SELECT * FROM variables WHERE app_id = @app_id ORDER BY key;

-- name: GetVariable :one
SELECT * FROM variables
WHERE owner_id = @owner_id AND app_id = @app_id AND key = @key;

-- name: UpsertVariableAndBump :one
WITH changed AS (
    INSERT INTO variables (owner_id, app_id, key, value, sealed, secret)
    VALUES (@owner_id, @app_id, @key, @value, @sealed, @secret)
    ON CONFLICT (app_id, key) DO UPDATE
    SET value      = excluded.value,
        sealed     = excluded.sealed,
        secret     = excluded.secret,
        updated_at = now()
    WHERE (variables.value, variables.sealed, variables.secret)
          IS DISTINCT FROM (excluded.value, excluded.sealed, excluded.secret)
    RETURNING variables.*
), bumped AS (
    UPDATE apps
    SET config_version = config_version + 1, updated_at = now()
    WHERE apps.owner_id = @owner_id AND apps.id = @app_id
      AND EXISTS (SELECT 1 FROM changed)
    RETURNING config_version
)
SELECT changed.id, changed.owner_id, changed.app_id, changed.key,
       changed.value, changed.sealed, changed.secret,
       changed.created_at, changed.updated_at, bumped.config_version
FROM changed CROSS JOIN bumped;

-- name: DeleteVariableAndBump :one
WITH removed AS (
    DELETE FROM variables
    WHERE variables.owner_id = @owner_id
      AND variables.app_id = @app_id AND variables.key = @key
    RETURNING secret
), bumped AS (
    UPDATE apps
    SET config_version = config_version + 1, updated_at = now()
    WHERE apps.owner_id = @owner_id AND apps.id = @app_id
      AND EXISTS (SELECT 1 FROM removed)
    RETURNING config_version
)
SELECT removed.secret, bumped.config_version
FROM removed CROSS JOIN bumped;

-- name: DeleteVariable :execrows
DELETE FROM variables
WHERE owner_id = @owner_id AND app_id = @app_id AND key = @key;
