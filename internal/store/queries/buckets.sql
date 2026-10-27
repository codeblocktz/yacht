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
