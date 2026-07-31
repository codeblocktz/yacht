-- Users and sessions carry no owner_id: a person exists before they belong to
-- any team. Memberships and invitations do, and stay scoped by it like
-- everything else.

-- name: UpsertUser :one
INSERT INTO users (email, display_name)
VALUES (lower(@email::text), @display_name)
ON CONFLICT (lower(email)) DO UPDATE
SET display_name = CASE
        WHEN excluded.display_name <> '' THEN excluded.display_name
        ELSE users.display_name
    END,
    updated_at = now()
RETURNING *;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE lower(email) = lower(@email::text);

-- name: GetUserByID :one
SELECT * FROM users WHERE id = @id;

-- name: CreateTeam :one
INSERT INTO teams (id, display_name)
VALUES (@id, @display_name)
ON CONFLICT (id) DO UPDATE SET display_name = excluded.display_name, updated_at = now()
RETURNING *;

-- name: GetTeam :one
SELECT * FROM teams WHERE id = @id;

-- A role change reads the owner count and then writes; taking the team row
-- first serialises those pairs, so two concurrent demotions cannot both see
-- two owners and both proceed.
-- name: LockTeam :one
SELECT * FROM teams WHERE id = @id FOR UPDATE;

-- name: UpsertMembership :one
INSERT INTO memberships (user_id, owner_id, role)
VALUES (@user_id, @owner_id, @role)
ON CONFLICT (user_id, owner_id) DO UPDATE SET role = excluded.role
RETURNING *;

-- name: GetMembership :one
SELECT * FROM memberships WHERE user_id = @user_id AND owner_id = @owner_id;

-- name: ListMembershipsForUser :many
SELECT m.*, t.display_name AS team_name
FROM memberships m
JOIN teams t ON t.id = m.owner_id
WHERE m.user_id = @user_id
ORDER BY t.display_name;

-- name: ListMembersOfTeam :many
SELECT m.*, u.email, u.display_name AS user_name
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.owner_id = @owner_id
ORDER BY u.email;

-- name: DeleteMembership :exec
DELETE FROM memberships WHERE user_id = @user_id AND owner_id = @owner_id;

-- name: CountOwnersOfTeam :one
SELECT count(*) FROM memberships WHERE owner_id = @owner_id AND role = 'owner';

-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, active_team_id, user_agent, ip, expires_at)
VALUES (@user_id, @token_hash, @active_team_id, @user_agent, @ip, @expires_at)
RETURNING *;

-- The team is joined in because the request that carries this cookie needs the
-- owner it resolves to, and a second round trip per request buys nothing. The
-- join is LEFT: a session whose team was deleted still exists, it just has no
-- owner to act as.
--
-- Expiry is filtered in SQL rather than in Go so that an expired row can never
-- be treated as valid by a caller that forgets to check.
-- name: GetSessionByHash :one
SELECT s.*, t.display_name AS team_name, t.email AS team_email
FROM sessions s
LEFT JOIN teams t ON t.id = s.active_team_id
WHERE s.token_hash = @token_hash AND s.expires_at > now();

-- name: GetSession :one
SELECT * FROM sessions WHERE id = @id;

-- name: SetSessionTeam :exec
UPDATE sessions SET active_team_id = @active_team_id WHERE id = @id;

-- name: DeleteSessionByHash :exec
DELETE FROM sessions WHERE token_hash = @token_hash;

-- name: DeleteSessionsForUser :exec
DELETE FROM sessions WHERE user_id = @user_id;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at <= now();
