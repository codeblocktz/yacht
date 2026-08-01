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

-- name: DeleteVariable :execrows
DELETE FROM variables
WHERE owner_id = @owner_id AND app_id = @app_id AND key = @key;
