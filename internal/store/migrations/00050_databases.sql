-- Databases: the arguments an engine is started with, recorded with each
-- release, and the backups taken of a database and restored into it.

-- +goose Up
-- +goose StatementBegin

-- An engine configured only on its command line — Redis takes its password as
-- a flag — needs the arguments in the release like everything else the pod
-- template is made of, so a rollback starts it the way it was started then.
-- Empty is the image's own command, which is what every release before this
-- column ran.
ALTER TABLE app_releases ADD COLUMN args text[] NOT NULL DEFAULT '{}';

-- One backup of a database, or one restore into it from a backup.
--
-- One table for both because they are the same kind of thing done to the same
-- data — a Job in the app's namespace that reads or writes a dump — and two of
-- them at once against one database is exactly what must not happen. The
-- partial unique index below is what refuses the second: an index holds across
-- every engine process, where a check in Go holds only in the one that made it.
CREATE TABLE database_backups (
    id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    app_id   uuid NOT NULL REFERENCES apps (id) ON DELETE CASCADE,

    kind    text NOT NULL CHECK (kind IN ('backup', 'restore')),
    trigger text NOT NULL DEFAULT 'manual' CHECK (trigger IN ('manual', 'scheduled')),
    status  text NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'succeeded', 'failed')),

    -- Where the dump is: 's3' is an object under the install's backup bucket,
    -- 'volume' a file on the app's backup claim. Stored per row rather than
    -- read from the configuration, so a backup taken before the install had a
    -- bucket is still found where it was written after it has one.
    location   text NOT NULL CHECK (location IN ('s3', 'volume')),
    object_key text NOT NULL DEFAULT '',

    -- A restore's source. Null once that backup has been deleted: the record
    -- that a restore happened outlives the dump it came from.
    backup_id uuid REFERENCES database_backups (id) ON DELETE SET NULL,

    job_name   text   NOT NULL DEFAULT '',
    size_bytes bigint NOT NULL DEFAULT 0,
    error      text   NOT NULL DEFAULT '',

    -- Who asked, as text rather than a reference to users, so the record
    -- outlives their account. Empty for the schedule.
    created_by text NOT NULL DEFAULT '',

    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);

CREATE INDEX database_backups_app_idx
    ON database_backups (app_id, started_at DESC);
CREATE INDEX database_backups_running_idx
    ON database_backups (status) WHERE status = 'running';

-- One job at a time against a database, whichever kind.
CREATE UNIQUE INDEX database_backups_one_running
    ON database_backups (app_id) WHERE status = 'running';

-- Whether a database is backed up every day. Its own table rather than a
-- column on apps: only databases have one, and most apps are not databases.
CREATE TABLE database_backup_schedules (
    app_id   uuid PRIMARY KEY REFERENCES apps (id) ON DELETE CASCADE,
    owner_id text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    daily    boolean NOT NULL DEFAULT false,
    -- How many scheduled backups are kept; older ones are deleted as each new
    -- one succeeds. Backups somebody took by hand are theirs to delete.
    keep     integer NOT NULL DEFAULT 7 CHECK (keep BETWEEN 1 AND 90),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS database_backup_schedules;
DROP TABLE IF EXISTS database_backups;
ALTER TABLE app_releases DROP COLUMN IF EXISTS args;
-- +goose StatementEnd
