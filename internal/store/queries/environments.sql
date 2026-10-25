-- name: CreateEnvironment :one
INSERT INTO environments (owner_id, project_id, slug, name, copied_from)
VALUES (@owner_id, @project_id, @slug, @name, @copied_from)
RETURNING *;

-- name: ListEnvironments :many
SELECT * FROM environments
WHERE owner_id = @owner_id AND project_id = @project_id
ORDER BY created_at, slug;

-- name: GetEnvironment :one
SELECT * FROM environments
WHERE owner_id = @owner_id AND project_id = @project_id AND slug = @slug;

-- name: DeleteEnvironment :execrows
DELETE FROM environments
WHERE owner_id = @owner_id AND project_id = @project_id AND slug = @slug;

-- name: ListAppsInEnvironment :many
SELECT * FROM apps
WHERE owner_id = @owner_id AND project_id = @project_id AND environment = @environment
ORDER BY name;

-- name: GetAppByService :one
-- The app that is a service in an environment, for a reference: ${{db.KEY}}
-- written in staging means staging's db.
SELECT * FROM apps
WHERE owner_id = @owner_id AND project_id = @project_id
  AND environment = @environment AND service = @service;

-- name: SetAppEnvironment :exec
-- Where an app made by Create belongs, written in Create's transaction.
UPDATE apps SET environment = @environment, service = @service
WHERE owner_id = @owner_id AND id = @id;
