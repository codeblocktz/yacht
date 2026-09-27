-- The install's capacity policy and the changes refused for it.
--
-- Install-wide, so unlike most queries here these take no owner_id — the same
-- as the cluster join settings. See the 00032 migration.

-- name: GetCapacityPolicy :one
SELECT * FROM capacity_policy WHERE id = 1;

-- Taken inside the transaction that commits a change, after the team's quota
-- row, when the install enforces its capacity. Every change that would raise
-- what the install has committed then runs one after another, so two creates
-- racing for the last room cannot both read it as free.
-- name: LockCapacityPolicy :one
SELECT * FROM capacity_policy WHERE id = 1 FOR UPDATE;

-- name: SetCapacityPolicy :one
INSERT INTO capacity_policy (
    id, enforce, cpu_commit_ratio, memory_commit_ratio,
    reserve_percent, warn_percent, storage_bytes
)
VALUES (
    1, @enforce, @cpu_commit_ratio, @memory_commit_ratio,
    @reserve_percent, @warn_percent, @storage_bytes
)
ON CONFLICT (id) DO UPDATE
SET enforce             = excluded.enforce,
    cpu_commit_ratio    = excluded.cpu_commit_ratio,
    memory_commit_ratio = excluded.memory_commit_ratio,
    reserve_percent     = excluded.reserve_percent,
    warn_percent        = excluded.warn_percent,
    storage_bytes       = excluded.storage_bytes,
    updated_at          = now()
RETURNING *;

-- Every app on the install, with its id so a change to one can be counted in
-- place. What the admission check totals; see ListAppFootprints for why the
-- arithmetic is in Go.
-- name: ListInstallFootprints :many
SELECT id, replicas, cpu_limit, memory_limit
FROM apps;

-- name: SumInstallVolumeBytes :one
SELECT coalesce(sum(size_bytes), 0)::bigint AS total
FROM volumes;

-- name: RecordCapacityRefusal :exec
INSERT INTO capacity_refusals (owner_id, resource, shortfall)
VALUES (@owner_id, @resource, @shortfall);

-- name: CountCapacityRefusalsSince :one
SELECT count(*)::bigint AS total
FROM capacity_refusals
WHERE at >= @since;

-- The operator's list, newest first, with the team's name as it is now.
-- name: ListCapacityRefusalsSince :many
SELECT r.id, r.owner_id, t.display_name, r.resource, r.shortfall, r.at
FROM capacity_refusals r
JOIN teams t ON t.id = r.owner_id
WHERE r.at >= @since
ORDER BY r.at DESC
LIMIT @max_rows;
