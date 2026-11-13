-- +goose Up
-- When the app holding a hostname came to hold it.
--
-- One ingress controller logs every team's requests into one stream, and an
-- app's HTTP log is that stream filtered to the app's hostnames. A hostname
-- changes hands — an app renamed, a generated name chosen again, an app
-- deleted, a custom domain removed — and whoever takes it next was shown the
-- previous holder's last day of client addresses, paths and query strings.
-- The log is read from here forward for each name.
--
-- created_at does not answer this for a platform hostname: its row is kept
-- when the hostname moves, and only the host column changes.
ALTER TABLE domains ADD COLUMN host_since timestamptz NOT NULL DEFAULT now();

-- A custom domain's row is made with its host and never given another, so
-- the claim is when it began. A platform hostname may have moved since its row
-- was made, and nothing recorded when; it starts from now, which costs the
-- app at most a day of its own history on the HTTP tab, once, rather than
-- showing anyone the history of a name somebody else held.
UPDATE domains SET host_since = created_at WHERE NOT managed;

-- +goose Down
ALTER TABLE domains DROP COLUMN host_since;
