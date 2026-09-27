-- What a team may commit, and what it has.
--
-- Committed CPU and memory cannot be summed here: a limit is stored as the
-- quantity string the cluster is given ("500m", "1Gi"), and an app with none
-- counts at the namespace default, which lives in Go. So these return each
-- app's replicas and limits and the service does the arithmetic, in one place.

-- name: GetTeamQuota :one
SELECT * FROM team_quotas WHERE owner_id = @owner_id;

-- Taken at the start of anything that would raise what a team has committed,
-- inside the transaction that commits it. Two creates racing each other then
-- run one after the other, and the second reads the first's app before it
-- decides — without this both read the same total, both fit, and together they
-- do not. No row means no limit, and nothing to race against.
-- name: LockTeamQuota :one
SELECT * FROM team_quotas WHERE owner_id = @owner_id FOR UPDATE;

-- name: UpsertTeamQuota :one
INSERT INTO team_quotas (owner_id, max_apps, cpu_millis, memory_bytes, storage_bytes)
VALUES (@owner_id, @max_apps, @cpu_millis, @memory_bytes, @storage_bytes)
ON CONFLICT (owner_id) DO UPDATE
SET max_apps      = excluded.max_apps,
    cpu_millis    = excluded.cpu_millis,
    memory_bytes  = excluded.memory_bytes,
    storage_bytes = excluded.storage_bytes,
    updated_at    = now()
RETURNING *;

-- name: ListAppFootprints :many
SELECT id, replicas, cpu_limit, memory_limit
FROM apps
WHERE owner_id = @owner_id;

-- name: SumVolumeBytes :one
SELECT coalesce(sum(size_bytes), 0)::bigint AS total
FROM volumes
WHERE owner_id = @owner_id;

-- Every team's apps at once, for the operator's list of teams.
--
-- The one query here with no owner_id filter, and deliberately: the page it
-- serves is the install's view across teams, gated to the operator the same
-- way the cluster's pod list is. One query rather than one per team, because
-- an install hosting many teams is exactly the install this page is for.
-- name: ListAllAppFootprints :many
SELECT owner_id, replicas, cpu_limit, memory_limit
FROM apps
ORDER BY owner_id;

-- Every team with its quota, its storage and how many people are in it. The
-- operator's list; see ListAllAppFootprints for why it crosses teams.
-- name: ListTeamQuotas :many
SELECT t.id, t.display_name,
       (SELECT count(*) FROM memberships m WHERE m.owner_id = t.id)::bigint AS members,
       (SELECT coalesce(sum(v.size_bytes), 0) FROM volumes v WHERE v.owner_id = t.id)::bigint AS storage_bytes,
       coalesce(q.max_apps, 0)::integer        AS quota_apps,
       coalesce(q.cpu_millis, 0)::bigint       AS quota_cpu_millis,
       coalesce(q.memory_bytes, 0)::bigint     AS quota_memory_bytes,
       coalesce(q.storage_bytes, 0)::bigint    AS quota_storage_bytes,
       q.updated_at                            AS quota_updated_at
FROM teams t
LEFT JOIN team_quotas q ON q.owner_id = t.id
ORDER BY lower(t.display_name), t.id;

-- name: GetTeamQuotaSummary :one
SELECT t.id, t.display_name,
       (SELECT count(*) FROM memberships m WHERE m.owner_id = t.id)::bigint AS members,
       (SELECT coalesce(sum(v.size_bytes), 0) FROM volumes v WHERE v.owner_id = t.id)::bigint AS storage_bytes,
       coalesce(q.max_apps, 0)::integer        AS quota_apps,
       coalesce(q.cpu_millis, 0)::bigint       AS quota_cpu_millis,
       coalesce(q.memory_bytes, 0)::bigint     AS quota_memory_bytes,
       coalesce(q.storage_bytes, 0)::bigint    AS quota_storage_bytes,
       q.updated_at                            AS quota_updated_at
FROM teams t
LEFT JOIN team_quotas q ON q.owner_id = t.id
WHERE t.id = @id;
