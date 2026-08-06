-- +goose Up
-- +goose StatementBegin

ALTER TABLE app_releases
    ADD COLUMN origin text NOT NULL DEFAULT 'deployment'
        CHECK (origin IN ('deployment', 'backfill'));

CREATE UNIQUE INDEX app_releases_one_backfill_per_app
    ON app_releases (app_id) WHERE origin = 'backfill';

-- Durable, resumable bookkeeping. The schema migration classifies only facts
-- already in PostgreSQL; registry access stays in the service process.
CREATE TABLE app_release_backfills (
    app_id       uuid PRIMARY KEY REFERENCES apps (id) ON DELETE CASCADE,
    owner_id     text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    state        text NOT NULL CHECK (state IN (
        'pending', 'ready', 'pending_image', 'never_deployed',
        'image_unavailable', 'blocked'
    )),
    last_error   text NOT NULL DEFAULT '',
    attempts     integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

INSERT INTO app_release_backfills (app_id, owner_id, state)
SELECT a.id, a.owner_id,
       CASE
           WHEN a.image = 'yacht.invalid/not-built-yet:pending' THEN 'pending_image'
           WHEN NOT EXISTS (
               SELECT 1 FROM deployments d
               WHERE d.app_id = a.id
                 AND d.status IN ('active', 'superseded', 'succeeded')
           ) THEN 'never_deployed'
           ELSE 'pending'
       END
FROM apps a
WHERE a.active_release_id IS NULL;

CREATE INDEX app_release_backfills_pending_idx
    ON app_release_backfills (next_attempt_at, app_id)
    WHERE state IN ('pending', 'image_unavailable', 'blocked');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS app_release_backfills;
DROP INDEX IF EXISTS app_releases_one_backfill_per_app;
ALTER TABLE app_releases DROP COLUMN IF EXISTS origin;
-- +goose StatementEnd
