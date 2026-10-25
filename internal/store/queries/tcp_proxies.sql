-- TCP proxies: a public port on every machine for one app's own port.

-- Claims a port for an app.
--
-- Not an upsert on port: the unique index is what makes two apps reaching for
-- the same port an error the caller retries with another, rather than one app
-- quietly taking the other's. An app that already has a proxy keeps it — the
-- port is what somebody's connection string says, and turning the proxy on
-- twice must not change it.
-- name: CreateTCPProxy :one
INSERT INTO tcp_proxies (owner_id, app_id, port)
VALUES (@owner_id, @app_id, @port)
ON CONFLICT (app_id) DO UPDATE SET port = tcp_proxies.port
RETURNING *;

-- name: GetTCPProxy :one
SELECT * FROM tcp_proxies
WHERE owner_id = @owner_id AND app_id = @app_id;

-- Read by the apply, which has the app and not always its owner's scope in
-- hand separately; the app id is unique on its own.
-- name: GetTCPProxyByApp :one
SELECT * FROM tcp_proxies
WHERE app_id = @app_id;

-- name: DeleteTCPProxy :execrows
DELETE FROM tcp_proxies
WHERE owner_id = @owner_id AND app_id = @app_id;

-- Every port in use, so a new one is picked from those that are not.
-- name: ListTCPProxyPorts :many
SELECT port FROM tcp_proxies ORDER BY port;
