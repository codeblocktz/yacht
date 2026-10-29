-- name: InsertBucket :exec
-- A bucket starts with the one byte it was made with (app.makeBucket), until
-- its limit is set.
INSERT INTO buckets (app_id, owner_id, garage_id, name, key_id, sealed_secret, limit_bytes)
VALUES ($1, $2, $3, $4, $5, $6, 1);

-- name: GetBucket :one
SELECT * FROM buckets WHERE owner_id = $1 AND app_id = $2;

-- name: ListBuckets :many
-- Every bucket on the install, for measuring.
SELECT * FROM buckets ORDER BY measured_at NULLS FIRST, app_id;

-- name: SetBucketUsage :exec
-- What a bucket holds, its unfinished uploads among it; a bucket that grew
-- wants room.
UPDATE buckets SET size_bytes = @size_bytes, objects = @objects, uploading_bytes = @uploading_bytes,
    wants_room_at = CASE WHEN @size_bytes::bigint > size_bytes THEN now() ELSE wants_room_at END,
    measured_at = now()
WHERE app_id = @app_id;

-- name: ListTeamBuckets :many
-- A team's buckets, with what each held when last measured.
SELECT * FROM buckets WHERE owner_id = $1 ORDER BY created_at, app_id;

-- name: SetBucketLimit :exec
UPDATE buckets SET limit_bytes = $2 WHERE app_id = $1;

-- name: BucketWantsRoom :exec
UPDATE buckets SET wants_room_at = now() WHERE app_id = $1;

-- name: ListBucketLimits :many
-- Every bucket on the install with what limits it: its size, the limit last
-- set, when it last wanted room, and its team's quota for buckets, zero for
-- none.
SELECT b.app_id, b.owner_id, b.garage_id, b.name, b.size_bytes, b.uploading_bytes,
       b.limit_bytes, b.wants_room_at, coalesce(q.bucket_bytes, 0)::bigint AS team_limit
FROM buckets b LEFT JOIN team_quotas q ON q.owner_id = b.owner_id
ORDER BY b.owner_id, b.created_at, b.app_id;

-- name: SumBuckets :one
-- Every bucket on the install: how many, and what they held when last
-- measured.
SELECT count(*)::bigint AS buckets, coalesce(sum(size_bytes), 0)::bigint AS stored
FROM buckets;
