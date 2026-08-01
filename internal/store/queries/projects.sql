-- Every query filters by owner_id, for the reason apps.sql gives: the scope is
-- the check, not a duplicate of one.

-- name: CreateProject :one
INSERT INTO projects (owner_id, slug, name)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetProjectBySlug :one
SELECT * FROM projects
WHERE owner_id = $1 AND slug = $2;

-- name: GetProjectByID :one
SELECT * FROM projects
WHERE owner_id = $1 AND id = $2;

-- name: ListProjects :many
SELECT p.*, count(a.id) AS app_count
FROM projects p
LEFT JOIN apps a ON a.project_id = p.id
WHERE p.owner_id = $1
GROUP BY p.id
ORDER BY p.name;

-- name: RenameProject :one
UPDATE projects
SET name = $3, updated_at = now()
WHERE owner_id = $1 AND id = $2
RETURNING *;

-- name: DeleteProject :execrows
DELETE FROM projects WHERE owner_id = $1 AND id = $2;

-- name: ListAppsInProject :many
SELECT * FROM apps
WHERE owner_id = $1 AND project_id = $2
ORDER BY name;

-- Apps that predate projects, or whose project was deleted.
--
-- Returned separately rather than folded into a default project by the query,
-- because assigning one is a write and a read should not have side effects
-- that a caller cannot see.
-- name: ListAppsWithoutProject :many
SELECT * FROM apps
WHERE owner_id = $1 AND project_id IS NULL
ORDER BY name;

-- name: SetAppProject :execrows
UPDATE apps
SET project_id = $3, updated_at = now()
WHERE owner_id = $1 AND id = $2;

-- name: MoveAppsWithoutProject :execrows
UPDATE apps
SET project_id = $2, updated_at = now()
WHERE owner_id = $1 AND project_id IS NULL;

-- name: SetAppPosition :execrows
UPDATE apps
SET canvas_x = $3, canvas_y = $4, updated_at = now()
WHERE owner_id = $1 AND name = $2;

-- Forget every saved position in a project, so the next render lays it out
-- again from the dependencies.
-- name: ClearProjectPositions :execrows
UPDATE apps
SET canvas_x = NULL, canvas_y = NULL, updated_at = now()
WHERE owner_id = $1 AND project_id = $2;
