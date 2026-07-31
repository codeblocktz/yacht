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

-- name: GetManagedDomain :one
SELECT * FROM domains
WHERE app_id = @app_id AND managed
LIMIT 1;
