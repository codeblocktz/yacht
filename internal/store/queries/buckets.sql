-- name: InsertBucket :exec
INSERT INTO buckets (app_id, owner_id, garage_id, name, key_id, sealed_secret)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: GetBucket :one
SELECT * FROM buckets WHERE owner_id = $1 AND app_id = $2;

-- name: ListBuckets :many
-- Every bucket on the install, for measuring.
SELECT * FROM buckets ORDER BY measured_at NULLS FIRST, app_id;

-- name: SetBucketUsage :exec
UPDATE buckets SET size_bytes = $2, objects = $3, measured_at = now()
WHERE app_id = $1;

-- name: ListTeamBuckets :many
-- A team's buckets, with what each held when last measured.
SELECT * FROM buckets WHERE owner_id = $1 ORDER BY created_at, app_id;

-- name: SetBucketLimit :exec
UPDATE buckets SET limit_bytes = $2 WHERE app_id = $1;

-- name: OtherBuckets :one
-- Every bucket on the install but a team's: how many, and how much more the
-- limits set on them let them write than they held when last measured. A
-- bucket with no limit set is counted, and gives nothing out.
SELECT count(*)::bigint AS buckets,
       coalesce(sum(greatest(limit_bytes - size_bytes, 0)) FILTER (WHERE limit_bytes > 0), 0)::bigint AS granted
FROM buckets WHERE owner_id <> $1;

-- name: SumBuckets :one
-- Every bucket on the install: how many, and what they held when last
-- measured.
SELECT count(*)::bigint AS buckets, coalesce(sum(size_bytes), 0)::bigint AS stored
FROM buckets;
