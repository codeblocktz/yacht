-- Users, sessions and credentials carry no owner_id: a person exists before they
-- belong to any team, and what proves who they are is not a fact about a tenant.
-- Memberships and invitations do, and stay scoped by it like everything else.

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

-- The columns are named rather than starred, and token_hash is not among them.
-- This list feeds the team page, and a hash that never leaves the database
-- cannot be rendered into it by a template that innocently prints a struct.
-- name: ListPendingInvitations :many
SELECT id, owner_id, email, role, expires_at, created_at
FROM invitations
WHERE owner_id = @owner_id AND accepted_at IS NULL AND expires_at > now()
ORDER BY email;

-- Scoped by owner_id as well as id: an id from another team must not be
-- reachable by someone who happens to administer this one.
-- name: DeleteInvitation :execrows
DELETE FROM invitations WHERE id = @id AND owner_id = @owner_id;

-- name: DeleteExpiredInvitations :exec
DELETE FROM invitations WHERE expires_at <= now() AND accepted_at IS NULL;

-- Reads an invitation without spending it, so a signed-out visitor can be sent
-- a sign-in link to the address it names. token_hash is not among the columns,
-- for the same reason it is absent from ListPendingInvitations.
-- name: GetInvitationByHash :one
SELECT id, owner_id, email, role, expires_at, created_at
FROM invitations
WHERE token_hash = @token_hash AND accepted_at IS NULL AND expires_at > now();

-- Withdraws the invitations somebody issued, used when they lose the authority
-- to have issued them. An administrator who leaves must not keep a live token
-- for an address they control.
-- name: DeleteInvitationsByInviter :exec
DELETE FROM invitations
WHERE owner_id = @owner_id AND invited_by = @invited_by::uuid AND accepted_at IS NULL;

-- ---------------------------------------------------------------------------
-- Passwords
--
-- Every query below either names its columns or returns an id. `secret` appears
-- in two of them — the sign-in lookup and the locking read that a change or a
-- removal goes through — and both row types are consumed inside the account
-- package and never returned from it. That is the whole storage rule, and it is
-- easier to keep by making the leak impossible than by remembering not to write
-- it. Anything added here that does not need the hash must not select it.
-- ---------------------------------------------------------------------------

-- The only query that returns a hash, and the only one that ever should.
--
-- The join is INNER, so an unknown address and a known address with no password
-- come back as the same pgx.ErrNoRows. There is no branch in Go that could tell
-- them apart, which is what makes the equal-cost verify in AuthenticatePassword
-- structural rather than something a caller has to remember to write.
--
-- The user's columns are named rather than starred, so this cannot quietly start
-- returning whatever is added to `users` next.
-- name: GetPasswordByEmail :one
SELECT c.secret,
       u.id, u.email, u.display_name, u.created_at, u.updated_at
FROM user_credentials c
JOIN users u ON u.id = c.user_id
WHERE c.kind = 'password' AND lower(u.email) = lower(@email::text);

-- name: HasPassword :one
SELECT EXISTS (
    SELECT 1 FROM user_credentials
    WHERE user_id = @user_id AND kind = 'password'
) AS has_password;

-- Reads and locks the person's password, for a caller inside a transaction that
-- is about to replace or remove it.
--
-- Returns the secret as well as the id because one row answers two questions the
-- caller needs at the same moment: whether this is an add or a replace, since
-- only a replace revokes the person's other sessions, and whether a current
-- password offered as proof of recent authentication is the right one. Asking
-- twice would be two round trips and one more chance for the second to read a
-- different row than the first.
--
-- FOR UPDATE is what serialises two tabs. Read outside the transaction this
-- would be check-then-act: both see no password, both insert, one upserts over
-- the other, and neither revokes.
-- name: LockPassword :one
SELECT id, secret FROM user_credentials
WHERE user_id = @user_id AND kind = 'password'
FOR UPDATE;

-- Returns an id and not the row. RETURNING * here would put the hash into a Go
-- struct for no reason at all, which is the shortest route to it being logged.
-- name: UpsertPassword :one
INSERT INTO user_credentials (user_id, kind, secret)
VALUES (@user_id, 'password', @secret)
ON CONFLICT (user_id, kind) DO UPDATE
SET secret = excluded.secret, updated_at = now()
RETURNING id;

-- execrows so that "there was nothing to remove" is distinguishable from
-- success. Reporting a credential withdrawn when none existed is how somebody
-- stops looking for the one that is still there.
-- name: DeletePassword :execrows
DELETE FROM user_credentials WHERE user_id = @user_id AND kind = 'password';

-- Rehash-on-login, for a stored hash made at a cost this build has moved past.
-- Best effort and never on the critical path: the sign-in has already succeeded
-- by the time this runs.
--
-- Conditional on the old value, which makes it a compare-and-swap. Between the
-- verify and this update the person may have changed their password in another
-- tab; without the condition, the rehash of the OLD password would overwrite the
-- new one and lock them out of an account they had just secured. With it, a
-- stale rehash matches no rows and nothing happens.
-- name: UpdatePasswordSecret :execrows
UPDATE user_credentials
SET secret = @secret, updated_at = now()
WHERE user_id = @user_id AND kind = 'password' AND secret = @old_secret;

-- ---------------------------------------------------------------------------
-- Step-up and session revocation
-- ---------------------------------------------------------------------------

-- Reads a session that is still alive, for a caller about to change how the
-- account it belongs to can be signed in to.
--
-- GetSession has no expiry filter, so an expired row comes back from it looking
-- valid. GetSessionByHash filters expiry in SQL precisely so that a caller
-- cannot forget, and this is the caller that would: a session that ended an hour
-- ago must not be able to set a password. FOR UPDATE serialises two tabs
-- changing one at the same moment.
--
-- Columns are named because the row exists to answer three questions — whose it
-- is, how recently it was proved, whether it is still alive — and starring it
-- would hand the caller a struct that grows every time `sessions` does.
-- name: GetLiveSession :one
SELECT id, user_id, active_team_id, authenticated_at, expires_at, created_at
FROM sessions
WHERE id = @id AND expires_at > now()
FOR UPDATE;

-- Opens the step-up window. Scoped by expiry so an expired session cannot be
-- revived into a recently-authenticated one; execrows so the caller can tell
-- that it was not.
-- name: TouchSessionAuthentication :execrows
UPDATE sessions SET authenticated_at = now()
WHERE id = @id AND expires_at > now();

-- Ends every session but the one asking.
--
-- DeleteSessionsForUser cannot serve here: it takes out the caller's own, so
-- changing a password from the account page would sign the person out of the
-- browser they are looking at, and they would read that as the change having
-- failed rather than having worked.
-- The surviving session is named `keep` rather than `except`, which is a
-- reserved word the query parser will not take as an identifier.
-- name: DeleteOtherSessionsForUser :exec
DELETE FROM sessions WHERE user_id = @user_id AND id <> @keep::uuid;
