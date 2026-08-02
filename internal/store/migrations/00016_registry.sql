-- +goose Up
-- +goose StatementBegin

-- Where images this install builds are pushed, and pulled back from.
--
-- No owner_id, for the reason cluster_join and platform_dns have none: this
-- configures one registry that every team's builds push to. It is also why the
-- repository prefix matters — one account holding every team's images needs
-- each app's path to be distinct, and that is derived rather than chosen.
--
-- One row by constraint. A second row nobody noticed is how half the builds
-- start pushing somewhere the cluster cannot pull from.
CREATE TABLE platform_registry (
    id integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),

    -- The registry host, e.g. ghcr.io or registry.example.com:5000. No scheme
    -- and no path: this is the part a container reference starts with, and
    -- anything else here produces an image name nothing can resolve.
    host text NOT NULL,

    -- The path images are pushed under, e.g. an organisation on ghcr.io.
    -- Empty is allowed: a registry that hands out the whole root does exist.
    repository text NOT NULL DEFAULT '',

    username text NOT NULL,

    -- Sealed with the same AES-GCM keeper that protects app secrets and the
    -- cluster join token. This credential can push images the cluster will
    -- then run, so a database dump must not be a way to get it.
    password_sealed bytea NOT NULL,

    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by uuid REFERENCES users (id) ON DELETE SET NULL
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS platform_registry;
-- +goose StatementEnd
