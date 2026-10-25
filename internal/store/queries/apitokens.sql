-- API tokens are a team's, like everything else scoped by owner_id. What a
-- token may do is the lesser of its own role and its maker's current one, so
-- the lookup joins the maker's membership: no membership, no token.

-- name: CreateAPIToken :one
INSERT INTO api_tokens (owner_id, project_id, name, role, token_hash, hint, origin, created_by)
VALUES (@owner_id, @project_id, @name, @role, @token_hash, @hint, @origin, @created_by)
RETURNING *;

-- name: ResolveAPIToken :one
SELECT t.*, m.role AS maker_role, tm.display_name AS team_name, tm.email AS team_email,
       u.email AS maker_email, p.slug AS project_slug
FROM api_tokens t
JOIN memberships m ON m.owner_id = t.owner_id AND m.user_id = t.created_by
JOIN teams tm ON tm.id = t.owner_id
JOIN users u ON u.id = t.created_by
LEFT JOIN projects p ON p.id = t.project_id
WHERE t.token_hash = @token_hash AND t.revoked_at IS NULL;

-- Throttled in the statement, so two requests racing to record the same
-- minute write once and the second does nothing.
-- name: TouchAPIToken :exec
UPDATE api_tokens
SET last_used_at = now(), last_used_ip = @ip
WHERE id = @id AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: ListAPITokens :many
SELECT t.*, u.email AS maker_email, p.slug AS project_slug, p.name AS project_name
FROM api_tokens t
JOIN users u ON u.id = t.created_by
LEFT JOIN projects p ON p.id = t.project_id
WHERE t.owner_id = @owner_id AND t.revoked_at IS NULL
ORDER BY t.created_at DESC;

-- name: GetAPIToken :one
SELECT * FROM api_tokens WHERE owner_id = @owner_id AND id = @id AND revoked_at IS NULL;

-- name: RevokeAPIToken :execrows
UPDATE api_tokens SET revoked_at = now()
WHERE owner_id = @owner_id AND id = @id AND revoked_at IS NULL;

-- name: CreateCLILogin :one
INSERT INTO cli_logins (device_hash, user_code, client_name, expires_at)
VALUES (@device_hash, @user_code, @client_name, @expires_at)
RETURNING *;

-- name: GetCLILoginByUserCode :one
SELECT * FROM cli_logins
WHERE user_code = @user_code AND expires_at > now() AND consumed_at IS NULL;

-- name: ApproveCLILogin :execrows
UPDATE cli_logins
SET approved_by = @approved_by, owner_id = @owner_id, role = @role, approved_at = now()
WHERE user_code = @user_code AND expires_at > now()
  AND consumed_at IS NULL AND approved_at IS NULL;

-- name: DenyCLILogin :execrows
UPDATE cli_logins SET denied = true, consumed_at = now()
WHERE user_code = @user_code AND expires_at > now() AND consumed_at IS NULL;

-- Taken under lock, so the token is minted once however many polls arrive
-- together.
-- name: LockCLILoginByDevice :one
SELECT * FROM cli_logins WHERE device_hash = @device_hash FOR UPDATE;

-- name: ConsumeCLILogin :exec
UPDATE cli_logins SET consumed_at = now() WHERE id = @id;

-- name: DeleteExpiredCLILogins :exec
DELETE FROM cli_logins WHERE expires_at < now() - interval '1 day';

-- name: ProjectBelongsToTeam :one
SELECT EXISTS (SELECT 1 FROM projects WHERE id = @id AND owner_id = @owner_id);
