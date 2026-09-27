-- Deploy-on-push webhooks. See migration 00028.

-- name: UpsertAppHook :one
-- A new secret replaces the old one outright, so regenerating is also how a
-- leaked secret is revoked.
INSERT INTO app_hooks (app_id, owner_id, secret)
VALUES (@app_id, @owner_id, @secret)
ON CONFLICT (app_id) DO UPDATE
SET secret = excluded.secret, created_at = now(),
    last_delivery_at = NULL, last_delivery = ''
RETURNING *;

-- name: GetAppHook :one
SELECT * FROM app_hooks WHERE owner_id = @owner_id AND app_id = @app_id;

-- name: DeleteAppHook :execrows
DELETE FROM app_hooks WHERE owner_id = @owner_id AND app_id = @app_id;

-- name: GetHookForDelivery :one
-- By app id alone: a delivery carries no session and no owner. The signature,
-- checked against the secret read here, is what authorises it.
SELECT sqlc.embed(a), h.secret
FROM app_hooks h
JOIN apps a ON a.id = h.app_id AND a.owner_id = h.owner_id
WHERE h.app_id = @app_id;

-- name: RecordHookDelivery :exec
UPDATE app_hooks
SET last_delivery_at = now(), last_delivery = @result
WHERE app_id = @app_id;

-- name: MarkHookPending :exec
UPDATE app_hooks
SET pending_at = now(), pending_revision = @revision
WHERE app_id = @app_id;

-- name: ListAdmissiblePushes :many
-- Pushes waiting behind a deploy that has since ended. An app whose deploy is
-- still live is left out, rather than tried and refused every half second.
SELECT sqlc.embed(a), h.pending_revision
FROM app_hooks h
JOIN apps a ON a.id = h.app_id AND a.owner_id = h.owner_id
WHERE h.pending_at IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM deployment_operations o
      WHERE o.app_id = h.app_id
        AND o.status IN ('queued', 'claimed', 'building', 'applying', 'verifying')
  )
ORDER BY h.pending_at
LIMIT @result_limit;

-- name: TakeHookPending :execrows
-- Cleared in the same transaction that admits the deploy, so a push is either
-- still waiting or has a deployment — never neither.
UPDATE app_hooks
SET pending_at = NULL, pending_revision = ''
WHERE app_id = @app_id AND pending_at IS NOT NULL;
