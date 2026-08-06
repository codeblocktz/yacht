-- Storage attached to an app. Scoped by owner_id like every other resource
-- query: a handler that forgets to scope produces no rows here rather than
-- another team's storage.

-- name: CreateVolume :one
INSERT INTO volumes (owner_id, app_id, name, mount_path, size_bytes, class)
VALUES (@owner_id, @app_id, @name, @mount_path, @size_bytes, @class)
RETURNING *;

-- name: ListVolumesForApp :many
SELECT * FROM volumes WHERE app_id = @app_id ORDER BY mount_path;

-- name: GetVolume :one
SELECT * FROM volumes
WHERE owner_id = @owner_id AND app_id = @app_id AND name = @name;

-- Expansion only, enforced in the WHERE clause rather than by reading the row
-- and comparing in Go: a check the caller performs is one a caller can skip,
-- and Kubernetes cannot shrink a claim afterwards to undo it.
-- name: GrowVolume :execrows
UPDATE volumes
SET size_bytes = @size_bytes, updated_at = now()
WHERE owner_id = @owner_id AND app_id = @app_id AND name = @name
  AND @size_bytes::bigint > size_bytes;

-- name: DeleteVolume :execrows
DELETE FROM volumes
WHERE owner_id = @owner_id AND app_id = @app_id AND name = @name;

-- name: CreateVolumeAndBump :one
WITH created AS (
    INSERT INTO volumes (owner_id, app_id, name, mount_path, size_bytes, class)
    VALUES (@owner_id, @app_id, @name, @mount_path, @size_bytes, @class)
    RETURNING volumes.*
), bumped AS (
    UPDATE apps
    SET config_version = config_version + 1, updated_at = now()
    WHERE apps.owner_id = @owner_id AND apps.id = @app_id
      AND EXISTS (SELECT 1 FROM created)
    RETURNING config_version
)
SELECT created.id, created.owner_id, created.app_id, created.name,
       created.mount_path, created.size_bytes, created.class,
       created.created_at, created.updated_at, bumped.config_version
FROM created CROSS JOIN bumped;

-- name: GrowVolumeAndBump :one
WITH grown AS (
    UPDATE volumes
    SET size_bytes = @size_bytes, updated_at = now()
    WHERE volumes.owner_id = @owner_id
      AND volumes.app_id = @app_id AND volumes.name = @name
      AND @size_bytes::bigint > size_bytes
    RETURNING volumes.*
), bumped AS (
    UPDATE apps
    SET config_version = config_version + 1, updated_at = now()
    WHERE apps.owner_id = @owner_id AND apps.id = @app_id
      AND EXISTS (SELECT 1 FROM grown)
    RETURNING config_version
)
SELECT grown.id, grown.owner_id, grown.app_id, grown.name,
       grown.mount_path, grown.size_bytes, grown.class,
       grown.created_at, grown.updated_at, bumped.config_version
FROM grown CROSS JOIN bumped;

-- name: DeleteVolumeAndBump :one
WITH removed AS (
    DELETE FROM volumes
    WHERE volumes.owner_id = @owner_id
      AND volumes.app_id = @app_id AND volumes.name = @name
    RETURNING volumes.*
), bumped AS (
    UPDATE apps
    SET config_version = config_version + 1, updated_at = now()
    WHERE apps.owner_id = @owner_id AND apps.id = @app_id
      AND EXISTS (SELECT 1 FROM removed)
    RETURNING config_version
)
SELECT removed.id, removed.owner_id, removed.app_id, removed.name,
       removed.mount_path, removed.size_bytes, removed.class,
       removed.created_at, removed.updated_at, bumped.config_version
FROM removed CROSS JOIN bumped;
