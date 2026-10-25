-- Public networking the way Railway draws it: no public address until somebody
-- asks for one, and a database reachable from outside through a TCP proxy.

-- +goose Up
-- +goose StatementBegin

-- A platform hostname under up.<app domain>, as opposed to one at
-- <name>.<app domain>.
--
-- Every app used to be given <name>.<app domain> the moment it was created.
-- Now an app has no public address until Generate Domain is pressed, and what
-- that issues lives one label further down, so a generated name cannot collide
-- with the dashboard, the registry, or anything else the install keeps at the
-- top of its domain. The hostnames already issued stay where they are — an
-- app live at one keeps serving there — and this column is what tells the two
-- kinds apart when the app domain changes and each has to be moved under it.
--
-- A column rather than a suffix match, for the reason managed is one: the app
-- domain is configuration and can change, and a row has to say what it was
-- when it was issued.
ALTER TABLE domains ADD COLUMN generated boolean NOT NULL DEFAULT false;

-- Only a platform hostname is generated. A custom domain is somebody's own
-- name, and has no place under the platform's.
ALTER TABLE domains ADD CONSTRAINT domains_generated_managed_check
    CHECK (NOT generated OR managed);

-- An app's TCP proxy: a port on every machine that reaches the app's own port,
-- for what does not speak HTTP — a database, mostly.
--
-- A table of its own rather than a column on apps. The port is what the
-- outside world dials, so it has to stay the same for as long as the proxy
-- exists, through every redeploy and recreation of the Service behind it; it is
-- chosen here, once, and handed to the cluster rather than read back from it.
CREATE TABLE tcp_proxies (
    app_id     uuid PRIMARY KEY REFERENCES apps (id) ON DELETE CASCADE,
    owner_id   text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,

    -- The public port. Unique across the install, because every machine
    -- answers on every proxy's port: two apps cannot share one.
    port       integer NOT NULL CHECK (port BETWEEN 1 AND 65535),

    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX tcp_proxies_port_key ON tcp_proxies (port);
CREATE INDEX tcp_proxies_owner_id_idx ON tcp_proxies (owner_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS tcp_proxies;
ALTER TABLE domains DROP CONSTRAINT IF EXISTS domains_generated_managed_check;
ALTER TABLE domains DROP COLUMN IF EXISTS generated;
-- +goose StatementEnd
