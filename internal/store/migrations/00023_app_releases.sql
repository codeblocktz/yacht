-- +goose Up
-- +goose StatementBegin

-- Immutable runtime snapshots. An app row remains the editable desired state;
-- this table records exactly what one deployment attempt meant to run.
CREATE TABLE app_releases (
    id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    app_id   uuid NOT NULL REFERENCES apps (id) ON DELETE CASCADE,

    -- Keep the human-facing reference as well as the immutable identity. The
    -- former explains where the release came from; the latter is what makes it
    -- reproducible after a tag moves.
    image_ref    text NOT NULL,
    image_digest text NOT NULL
        CHECK (image_digest ~ '^sha256:[0-9a-f]{64}$'),

    source          text NOT NULL,
    source_revision text NOT NULL DEFAULT '',

    replicas       integer NOT NULL CHECK (replicas >= 0),
    port           integer NOT NULL CHECK (port >= 0 AND port <= 65535),
    cpu_request    text NOT NULL DEFAULT '',
    cpu_limit      text NOT NULL DEFAULT '',
    memory_request text NOT NULL DEFAULT '',
    memory_limit   text NOT NULL DEFAULT '',
    internal       boolean NOT NULL DEFAULT false,

    run_as_user             bigint NOT NULL DEFAULT 0,
    fs_group                bigint NOT NULL DEFAULT 0,
    scratch_paths           text[] NOT NULL DEFAULT '{}',
    writable_root_filesystem boolean NOT NULL DEFAULT false,

    health_path     text NOT NULL DEFAULT ''
        CHECK (health_path = '' OR health_path LIKE '/%'),
    health_liveness boolean NOT NULL DEFAULT false,

    -- Secret values deliberately do not belong to history. The names are
    -- enough to validate that the current overlay can satisfy the release.
    env         jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(env) = 'object'),
    secret_keys text[] NOT NULL DEFAULT '{}',

    -- The editable generation this snapshot came from. Later activation and
    -- rollback use it for optimistic conflict reporting.
    config_version bigint NOT NULL CHECK (config_version > 0),

    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX app_releases_owner_idx ON app_releases (owner_id);
CREATE INDEX app_releases_app_created_idx
    ON app_releases (app_id, created_at DESC, id DESC);

-- No application query updates a release, and the database enforces the same
-- invariant. Deletion remains possible only through the app's ON DELETE
-- CASCADE, so removing an app still removes its history.
CREATE FUNCTION reject_app_release_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'app releases are immutable';
END;
$$;

CREATE TRIGGER app_releases_immutable
BEFORE UPDATE ON app_releases
FOR EACH ROW EXECUTE FUNCTION reject_app_release_update();

ALTER TABLE apps
    ADD COLUMN config_version bigint NOT NULL DEFAULT 1
        CHECK (config_version > 0),
    ADD COLUMN active_release_id uuid;

ALTER TABLE apps
    ADD CONSTRAINT apps_active_release_fk
    FOREIGN KEY (active_release_id) REFERENCES app_releases (id)
    ON DELETE SET NULL DEFERRABLE INITIALLY IMMEDIATE;

ALTER TABLE deployments
    ADD COLUMN release_id uuid REFERENCES app_releases (id) ON DELETE SET NULL,
    ADD COLUMN trigger text NOT NULL DEFAULT 'manual',
    ADD COLUMN actor_kind text NOT NULL DEFAULT 'system'
        CHECK (actor_kind IN ('system', 'user', 'token', 'webhook')),
    ADD COLUMN actor_id text NOT NULL DEFAULT '';

CREATE INDEX deployments_release_idx ON deployments (release_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS deployments_release_idx;
ALTER TABLE deployments
    DROP COLUMN IF EXISTS actor_id,
    DROP COLUMN IF EXISTS actor_kind,
    DROP COLUMN IF EXISTS trigger,
    DROP COLUMN IF EXISTS release_id;

ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_active_release_fk;
ALTER TABLE apps
    DROP COLUMN IF EXISTS active_release_id,
    DROP COLUMN IF EXISTS config_version;

DROP TRIGGER IF EXISTS app_releases_immutable ON app_releases;
DROP FUNCTION IF EXISTS reject_app_release_update();
DROP TABLE IF EXISTS app_releases;
-- +goose StatementEnd
