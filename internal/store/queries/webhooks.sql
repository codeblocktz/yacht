-- A project's webhooks and their deliveries, scoped by owner like every
-- other table, and by project where a page reads them.

-- name: CreateProjectWebhook :one
INSERT INTO project_webhooks (owner_id, project_id, url, events, header_names, headers_sealed, secret_sealed, created_by)
VALUES (@owner_id, @project_id, @url, @events, @header_names, @headers_sealed, @secret_sealed, @created_by)
RETURNING *;

-- name: ListProjectWebhooks :many
SELECT * FROM project_webhooks
WHERE owner_id = @owner_id AND project_id = @project_id
ORDER BY created_at;

-- name: GetProjectWebhook :one
SELECT * FROM project_webhooks WHERE owner_id = @owner_id AND id = @id;

-- name: DeleteProjectWebhook :execrows
DELETE FROM project_webhooks WHERE owner_id = @owner_id AND id = @id;

-- name: WebhooksForEvent :many
SELECT * FROM project_webhooks
WHERE owner_id = @owner_id AND project_id = @project_id AND @event::text = ANY (events);

-- name: CreateWebhookDelivery :one
INSERT INTO webhook_deliveries (webhook_id, owner_id, event, payload)
VALUES (@webhook_id, @owner_id, @event, @payload)
RETURNING *;

-- Claimed with SKIP LOCKED and a lease, so replicas share the queue without
-- sending anything twice; a process that dies mid-send leaves a lease that
-- runs out and the delivery is tried again.
-- name: ClaimDueWebhookDeliveries :many
UPDATE webhook_deliveries
SET locked_until = now() + interval '2 minutes'
WHERE id IN (
    SELECT id FROM webhook_deliveries
    WHERE status = 'pending' AND next_attempt_at <= now()
      AND (locked_until IS NULL OR locked_until < now())
    ORDER BY next_attempt_at
    LIMIT @batch
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: ClaimWebhookDelivery :one
UPDATE webhook_deliveries
SET locked_until = now() + interval '2 minutes'
WHERE id = @id AND status = 'pending' AND (locked_until IS NULL OR locked_until < now())
RETURNING *;

-- name: RecordWebhookAttempt :exec
UPDATE webhook_deliveries
SET attempts = attempts + 1,
    status = @status,
    next_attempt_at = @next_attempt_at,
    last_status = @last_status,
    last_error = @last_error,
    last_response = @last_response,
    last_duration_ms = @last_duration_ms,
    locked_until = NULL,
    finished_at = CASE WHEN @status::text = 'pending' THEN NULL ELSE now() END
WHERE id = @id;

-- name: ListWebhookDeliveries :many
SELECT * FROM webhook_deliveries
WHERE owner_id = @owner_id AND webhook_id = @webhook_id
ORDER BY created_at DESC
LIMIT @lim;

-- name: PruneWebhookDeliveries :exec
DELETE FROM webhook_deliveries WHERE finished_at < @before;

-- Flips the app's recorded health, returning the row only when it changed,
-- so of every replica looking at the same app only one hears it start failing.
-- name: SetAppFailing :one
INSERT INTO webhook_app_health (app_id, owner_id, failing, changed_at)
VALUES (@app_id, @owner_id, @failing, now())
ON CONFLICT (app_id) DO UPDATE SET failing = excluded.failing, changed_at = now()
WHERE webhook_app_health.failing IS DISTINCT FROM excluded.failing
RETURNING app_id, failing, (xmax = 0) AS inserted;

-- Teams with somebody listening for app.crashed, so the watcher only looks
-- at apps whose failing somebody would be told about. Only apps that have
-- served: one whose first deploy has not succeeded has nothing that could
-- start crashing, and its deploy says how it went.
-- name: AppsWatchedForCrashes :many
SELECT a.id, a.owner_id, a.name, a.namespace, a.project_id, p.slug AS project_slug, p.name AS project_name,
       a.active_release_id::uuid AS active_release_id
FROM apps a
JOIN projects p ON p.id = a.project_id
WHERE a.active_release_id IS NOT NULL
AND EXISTS (
    SELECT 1 FROM project_webhooks w
    WHERE w.project_id = a.project_id AND 'app.crashed' = ANY (w.events)
);
