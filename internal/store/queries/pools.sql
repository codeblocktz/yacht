-- Which pool of machines a team's apps are placed on. No row is anywhere.

-- name: GetTeamPool :one
SELECT * FROM team_pools WHERE owner_id = @owner_id;

-- Taken before a pool is changed, inside the transaction that changes it, so
-- two changes racing each other each compare against what the other wrote.
-- name: LockTeamPool :one
SELECT * FROM team_pools WHERE owner_id = @owner_id FOR UPDATE;

-- name: UpsertTeamPool :one
INSERT INTO team_pools (owner_id, pool)
VALUES (@owner_id, @pool)
ON CONFLICT (owner_id) DO UPDATE
SET pool       = excluded.pool,
    updated_at = now()
RETURNING *;

-- A team's pool is a live overlay on every one of its apps: rendered into each
-- pod template, owned by no release. One change invalidates each of the team's
-- apps exactly once, and the reconciler rolls them onto the pool without
-- manufacturing release history. A sleeping app is re-applied at zero
-- replicas and wakes onto the pool.
-- name: IncrementTeamAppConfigVersions :execrows
UPDATE apps
SET config_version = config_version + 1, updated_at = now()
WHERE owner_id = @owner_id;
