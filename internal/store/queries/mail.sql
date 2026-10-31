-- name: RecordMailDelivery :exec
INSERT INTO mail_deliveries (sent_at, error) VALUES (@sent_at, sqlc.narg(error));

-- name: PruneMailDeliveries :exec
DELETE FROM mail_deliveries WHERE sent_at < @before;

-- name: LastMailSent :one
SELECT coalesce(max(sent_at), 'epoch'::timestamptz)::timestamptz AS sent_at
FROM mail_deliveries WHERE error IS NULL;

-- name: LastMailFailure :many
SELECT sent_at, error::text AS error
FROM mail_deliveries WHERE error IS NOT NULL
ORDER BY sent_at DESC, id DESC LIMIT 1;

-- name: CountMailFailuresSince :one
SELECT count(*) FROM mail_deliveries WHERE error IS NOT NULL AND sent_at >= @since;
