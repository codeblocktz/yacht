-- The rest of an app's Deploy and Build settings: a cron schedule, a restart
-- policy, a healthcheck timeout, a Dockerfile path and watch paths.
--
-- Every default is what an app did before the setting existed: no schedule
-- keeps it running, "on_failure" with no retry count of its own keeps the
-- engine's crash limit, a zero timeout keeps the engine's rollout budget, an
-- empty Dockerfile path builds the Dockerfile at the root, and no watch paths
-- deploys every push.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE apps
    -- A cron expression, checked by the engine with the parser Kubernetes
    -- uses. Bounded only for length here, like a region: two copies of a
    -- grammar drift.
    ADD COLUMN cron_schedule text NOT NULL DEFAULT ''
        CHECK (length(cron_schedule) <= 128),
    ADD COLUMN restart_policy text NOT NULL DEFAULT 'on_failure'
        CHECK (restart_policy IN ('on_failure', 'always', 'never')),
    -- Zero is the engine's own limit; see crashesBeforeFailing.
    ADD COLUMN restart_max_retries integer NOT NULL DEFAULT 0
        CHECK (restart_max_retries BETWEEN 0 AND 100),
    -- Zero is the engine's rollout budget.
    ADD COLUMN health_timeout_seconds integer NOT NULL DEFAULT 0
        CHECK (health_timeout_seconds BETWEEN 0 AND 1800),
    -- Build settings, read by the next build like the repository is, and so
    -- on the app only: a release records what an image was run with, not
    -- how it was made.
    ADD COLUMN dockerfile_path text NOT NULL DEFAULT ''
        CHECK (length(dockerfile_path) <= 255),
    ADD COLUMN watch_paths text[] NOT NULL DEFAULT '{}';

-- The runtime ones in the release as well, so a rollback runs the release on
-- the schedule, with the restarts and the patience it was deployed with.
ALTER TABLE app_releases
    ADD COLUMN cron_schedule text NOT NULL DEFAULT '',
    ADD COLUMN restart_policy text NOT NULL DEFAULT 'on_failure',
    ADD COLUMN restart_max_retries integer NOT NULL DEFAULT 0,
    ADD COLUMN health_timeout_seconds integer NOT NULL DEFAULT 0;

-- A push that changed none of an app's watch paths is recorded as a
-- deployment that did not happen, the way Railway lists it, rather than
-- disappearing: "why did my push not deploy" is answered where somebody
-- looks for the deploy.
ALTER TABLE deployments DROP CONSTRAINT IF EXISTS deployments_status_check;
ALTER TABLE deployments ADD CONSTRAINT deployments_status_check
    CHECK (status IN (
        'pending', 'running', 'active', 'succeeded',
        'failed', 'cancelled', 'superseded', 'skipped'
    ));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM deployments WHERE status = 'skipped';
ALTER TABLE deployments DROP CONSTRAINT IF EXISTS deployments_status_check;
ALTER TABLE deployments ADD CONSTRAINT deployments_status_check
    CHECK (status IN (
        'pending', 'running', 'active', 'succeeded',
        'failed', 'cancelled', 'superseded'
    ));
ALTER TABLE app_releases
    DROP COLUMN IF EXISTS health_timeout_seconds,
    DROP COLUMN IF EXISTS restart_max_retries,
    DROP COLUMN IF EXISTS restart_policy,
    DROP COLUMN IF EXISTS cron_schedule;
ALTER TABLE apps
    DROP COLUMN IF EXISTS watch_paths,
    DROP COLUMN IF EXISTS dockerfile_path,
    DROP COLUMN IF EXISTS health_timeout_seconds,
    DROP COLUMN IF EXISTS restart_max_retries,
    DROP COLUMN IF EXISTS restart_policy,
    DROP COLUMN IF EXISTS cron_schedule;
-- +goose StatementEnd
