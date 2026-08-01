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
