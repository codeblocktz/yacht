-- Platform-issued hostnames. Custom domains get their own queries when that
-- sub-project lands; keeping them in one file rather than in apps.sql is so
-- that apps.sql stays about apps.

-- Issues or reissues the platform hostname for an app.
--
-- The conflict target is the partial index, so a second call for the same app
-- moves its hostname rather than adding one. A collision on the global host
-- index is a different conflict and is deliberately left to raise, because two
-- apps claiming one hostname is a real error, not something to paper over.
--
-- verified is true because a name the platform issued needs no proof of
-- ownership; requiring proof would mean inventing a verification step for a
-- name we already control.
-- name: UpsertManagedDomain :one
INSERT INTO domains (owner_id, app_id, host, tls, verified, managed)
VALUES (@owner_id, @app_id, lower(@host), @tls, true, true)
ON CONFLICT (app_id) WHERE managed
DO UPDATE SET host = lower(@host), tls = @tls
RETURNING *;

-- name: ListDomainsByApp :many
SELECT * FROM domains
WHERE app_id = @app_id
ORDER BY managed DESC, host;

-- Releases an app's platform hostname.
--
-- Deleting rather than leaving the row is what makes the feature reversible:
-- hostnames are globally unique, so a retired row keeps the name reserved
-- against every other app forever.
-- name: DeleteManagedDomain :exec
DELETE FROM domains
WHERE app_id = @app_id AND managed;

-- name: GetManagedDomain :one
SELECT * FROM domains
WHERE app_id = @app_id AND managed
LIMIT 1;

-- ---------------------------------------------------------- custom domains

-- Claims a hostname for an app, unverified.
--
-- Deliberately not an upsert on host: the global unique index is what makes two
-- teams claiming one name an error rather than a silent transfer, and papering
-- over it here is exactly how a domain gets stolen.
-- name: CreateCustomDomain :one
INSERT INTO domains (owner_id, app_id, host, tls, verified, managed, verify_target)
VALUES (@owner_id, @app_id, lower(@host), true, false, false, @verify_target)
RETURNING *;

-- name: ListCustomDomains :many
SELECT * FROM domains
WHERE owner_id = @owner_id AND app_id = @app_id AND NOT managed
ORDER BY host;

-- Marks a claim proven. Scoped by owner as well as id: verifying is a write,
-- and a write on somebody else's row is the thing owner scoping exists for.
-- name: VerifyCustomDomain :execrows
UPDATE domains
SET verified = true, verified_at = now()
WHERE owner_id = @owner_id AND id = @id AND NOT managed;

-- name: DeleteCustomDomain :execrows
DELETE FROM domains
WHERE owner_id = @owner_id AND id = @id AND NOT managed;

-- name: GetCustomDomain :one
SELECT * FROM domains
WHERE owner_id = @owner_id AND id = @id AND NOT managed;

-- Hostnames that may actually be routed to.
--
-- A managed host is routable because the platform issued it; a custom one only
-- once it is proven. This is the query the Ingress is built from, so the gate
-- lives here rather than in a caller that might forget it.
-- name: RoutableHostsForApp :many
SELECT host FROM domains
WHERE app_id = @app_id AND (managed OR verified)
ORDER BY managed DESC, host;
