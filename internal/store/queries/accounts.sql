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
-- Resolves a session only while the membership behind it still exists.
--
-- The join to memberships is the security boundary, not decoration. Revoking
-- sessions imperatively when someone is removed would work only for the call
-- sites that remember to do it, and would still miss membership lost by
-- cascade. Joining makes a departed member's cookie stop resolving the instant
-- the row goes, by whatever route it went.
--
-- INNER JOIN on teams too: a session with no active team resolves to no owner,
-- and returning a row the caller must then remember to reject is how that
-- check gets skipped.
-- name: GetSessionByHash :one
SELECT s.*, t.display_name AS team_name, t.email AS team_email, m.role AS member_role
FROM sessions s
JOIN teams t ON t.id = s.active_team_id
JOIN memberships m ON m.owner_id = s.active_team_id AND m.user_id = s.user_id
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

-- name: CreateMagicLink :one
INSERT INTO magic_links (user_id, token_hash, expires_at)
VALUES (@user_id, @token_hash, @expires_at)
RETURNING *;

-- Marking consumed and reading the user are one statement, so there is no
-- window between the two in which a second request can also find the link
-- unconsumed. Expiry is in the same condition for the same reason: a link that
-- has just expired must fail here rather than in whatever checks it later.
-- name: ConsumeMagicLink :one
WITH consumed AS (
    UPDATE magic_links
    SET consumed_at = now()
    WHERE token_hash = @token_hash
      AND consumed_at IS NULL
      AND expires_at > now()
    RETURNING user_id
)
SELECT u.* FROM users u JOIN consumed ON consumed.user_id = u.id;

-- name: DeleteExpiredMagicLinks :exec
DELETE FROM magic_links WHERE expires_at <= now();

-- Re-inviting replaces the pending invitation rather than adding a second one,
-- so the token in the older mail stops working. Two live tokens for one address
-- would mean revoking the invitation on screen leaves the other one usable.
-- name: UpsertInvitation :one
INSERT INTO invitations (owner_id, email, role, token_hash, invited_by, expires_at)
VALUES (@owner_id, lower(@email::text), @role, @token_hash, @invited_by::uuid, @expires_at)
ON CONFLICT (owner_id, lower(email)) WHERE accepted_at IS NULL
DO UPDATE SET role       = excluded.role,
              token_hash = excluded.token_hash,
              invited_by = excluded.invited_by,
              expires_at = excluded.expires_at,
              created_at = now()
RETURNING id;

-- One conditional UPDATE, like the magic link: checking first and writing after
-- leaves a window in which the same invitation is accepted twice.
-- name: AcceptInvitation :one
UPDATE invitations
SET accepted_at = now()
WHERE token_hash = @token_hash
  AND accepted_at IS NULL
  AND expires_at > now()
RETURNING owner_id, role, email;

-- Scoped by owner_id as well as id: an id from another team must not be
-- reachable by someone who happens to administer this one.
-- name: DeleteInvitation :execrows
DELETE FROM invitations WHERE id = @id AND owner_id = @owner_id;

-- name: DeleteExpiredInvitations :exec
DELETE FROM invitations WHERE expires_at <= now() AND accepted_at IS NULL;
