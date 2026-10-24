-- Regions are the install's, like the capacity policy: what a region is
-- called and where its traffic enters is the same for every team.

-- name: ListRegions :many
SELECT * FROM regions ORDER BY name;

-- name: GetRegion :one
SELECT * FROM regions WHERE name = @name;

-- name: UpsertRegion :one
INSERT INTO regions (name, display_name, edge_host, edge_addresses, storage_class, open)
VALUES (@name, @display_name, @edge_host, @edge_addresses, @storage_class, @open)
ON CONFLICT (name) DO UPDATE
SET display_name   = excluded.display_name,
    edge_host      = excluded.edge_host,
    edge_addresses = excluded.edge_addresses,
    storage_class  = excluded.storage_class,
    open           = excluded.open,
    updated_at     = now()
RETURNING *;

-- The install's default region gets a row, open, the first time it is named,
-- so a cluster labelled with one region and YACHT_DEFAULT_REGION set needs
-- nothing more before apps can be placed. Never overwrites: an operator who
-- closed it, or named it, keeps that.
-- name: EnsureRegion :exec
INSERT INTO regions (name) VALUES (@name)
ON CONFLICT (name) DO NOTHING;

-- How many apps each region holds, across every team, for the operator. Null
-- is the apps not placed in any region yet.
-- name: CountAppsByRegion :many
SELECT region, count(*)::bigint AS apps
FROM apps
GROUP BY region;

-- Moves an app to a region and versions it, so the reconciler re-applies it
-- with the new placement the way a team's pool change does.
-- name: SetAppRegion :one
UPDATE apps
SET region = @region, config_version = config_version + 1, updated_at = now()
WHERE owner_id = @owner_id AND id = @id
RETURNING *;

-- Records the region an app already lives in, once. Not versioned: recording
-- where an app is changes nothing about it, and the affinity reaches its pods
-- at its next apply rather than restarting every app on the install at once.
-- Only where it is still unrecorded, so a move racing this is never undone.
-- name: RecordAppRegion :execrows
UPDATE apps
SET region = @region
WHERE owner_id = @owner_id AND id = @id AND region IS NULL;

-- Every app not yet placed in a region, with whether it has storage: an app
-- with a volume is never recorded anywhere but where it is seen running,
-- because its volume may live on one region's machines and nowhere else.
-- name: ListUnplacedApps :many
SELECT a.id, a.owner_id, a.namespace, a.name,
       EXISTS (SELECT 1 FROM volumes v WHERE v.app_id = a.id)::boolean AS has_volumes
FROM apps a
WHERE a.region IS NULL
ORDER BY a.created_at;

-- Every hostname on the install with the app and region it belongs to, for an
-- application that writes DNS. Not owner-scoped: it is the wrapper's view of
-- the whole install, like the capacity footprints.
-- name: ListRoutes :many
SELECT d.host, d.managed, d.state, d.verified,
       a.id AS app_id, a.owner_id, a.name AS app_name, a.region
FROM domains d
JOIN apps a ON a.id = d.app_id
ORDER BY d.host;
