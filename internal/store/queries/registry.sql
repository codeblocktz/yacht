-- The install's image registry. No owner_id, for the reason platform_dns has
-- none: one registry serves every team's builds.

-- name: GetPlatformRegistry :one
SELECT * FROM platform_registry WHERE id = 1;

-- name: SetPlatformRegistry :one
INSERT INTO platform_registry (id, host, repository, username, password_sealed, updated_by)
VALUES (1, @host, @repository, @username, @password_sealed, @updated_by)
ON CONFLICT (id) DO UPDATE
    SET host            = EXCLUDED.host,
        repository      = EXCLUDED.repository,
        username        = EXCLUDED.username,
        password_sealed = EXCLUDED.password_sealed,
        updated_at      = now(),
        updated_by      = EXCLUDED.updated_by
RETURNING *;

-- name: ClearPlatformRegistry :exec
DELETE FROM platform_registry WHERE id = 1;
