-- Apps that sleep when nobody is using them, and wake on the next request.
--
-- Most small apps are idle most of the day. One that has had no requests for
-- a while is scaled to zero and its hostnames routed to the engine's waker,
-- which scales it back up when somebody asks for it. The room it gave back is
-- counted as free, less a reserve kept so the sleepers can still wake.

-- +goose Up
-- +goose StatementBegin

-- An app's own choice, and where it stands.
--
-- sleep_mode 'inherit' takes the team's default, which is off until somebody
-- turns it on — so every app that existed before this keeps running exactly
-- as it did. sleep_after_minutes is the app's own idle time; NULL takes the
-- team's.
--
-- sleep_state is 'waking' between the pods being asked for and the route
-- being switched back to them; sleeping_since is when it fell asleep and
-- stays set through the wake, so the interval it closes is known.
--
-- awake_since, last_request_at and requests_seen_since together are the idle
-- clock: an app is idle since the latest of them. requests_seen_since is when
-- request data first reached this app at all — an app nobody could count
-- requests for is never idle, however quiet it looks. request_count is the
-- ingress controller's counter as last read, compared rather than trusted: a
-- counter that moved, either way, is somebody using the app.
ALTER TABLE apps
    ADD COLUMN sleep_mode text NOT NULL DEFAULT 'inherit'
        CHECK (sleep_mode IN ('inherit', 'on', 'off')),
    ADD COLUMN sleep_after_minutes integer
        CHECK (sleep_after_minutes IS NULL OR sleep_after_minutes BETWEEN 5 AND 10080),
    ADD COLUMN sleep_state text NOT NULL DEFAULT 'awake'
        CHECK (sleep_state IN ('awake', 'asleep', 'waking')),
    ADD COLUMN sleeping_since timestamptz,
    ADD COLUMN waking_since timestamptz,
    ADD COLUMN awake_since timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN last_request_at timestamptz,
    ADD COLUMN request_count bigint,
    ADD COLUMN requests_seen_since timestamptz,
    -- The last wake that did not happen, and why: 'no-room' or 'timeout'. The
    -- waker answers from it for a minute rather than trying again on every
    -- reload of a page that said "try again in a minute".
    ADD COLUMN wake_failed_at timestamptz,
    ADD COLUMN wake_failure text NOT NULL DEFAULT ''
        CHECK (wake_failure IN ('', 'no-room', 'timeout')),
    ADD CONSTRAINT apps_sleeping_since_check
        CHECK ((sleep_state = 'awake') = (sleeping_since IS NULL)),
    ADD CONSTRAINT apps_waking_since_check
        CHECK ((sleep_state = 'waking') = (waking_since IS NOT NULL));

-- A team's default, for every app of its left at 'inherit'. A hosting
-- business turns this on for its small plans; an install that never touches
-- it has no rows and no app sleeps.
CREATE TABLE team_sleep_defaults (
    owner_id      text PRIMARY KEY REFERENCES teams (id) ON DELETE CASCADE,
    enabled       boolean NOT NULL DEFAULT false,
    after_minutes integer NOT NULL DEFAULT 30 CHECK (after_minutes BETWEEN 5 AND 10080),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Every time an app slept, and when and why it woke. The app's own history of
-- it, and what an application wrapping the engine reads to stop charging for
-- an app while it is asleep.
--
-- idle_minutes is the idle time it slept after; NULL is somebody putting it to
-- sleep by hand. woke_at is when its pods were asked for again — from then it
-- is using the machines — and is NULL while it is still asleep.
CREATE TABLE app_sleeps (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id     text        NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    app_id       uuid        NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    slept_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    idle_minutes integer,
    woke_at      timestamptz,
    woke_by      text CHECK (woke_by IN ('request', 'manual', 'deploy', 'install')),
    CHECK ((woke_at IS NULL) = (woke_by IS NULL)),
    CHECK (woke_at IS NULL OR woke_at >= slept_at)
);

-- At most one open interval per app: an app is asleep once, not twice.
CREATE UNIQUE INDEX app_sleeps_open_idx ON app_sleeps (app_id) WHERE woke_at IS NULL;
CREATE INDEX app_sleeps_owner_idx ON app_sleeps (owner_id, slept_at DESC);

-- How much room is kept for sleeping apps to wake: a sleeper is counted at
-- this share of its size, so the install never sells so much that the
-- sleepers cannot come back. 25 keeps room for a quarter of them at once.
ALTER TABLE capacity_policy
    ADD COLUMN wake_reserve_percent integer NOT NULL DEFAULT 25
        CHECK (wake_reserve_percent >= 0 AND wake_reserve_percent <= 100);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE capacity_policy DROP COLUMN IF EXISTS wake_reserve_percent;
DROP TABLE IF EXISTS app_sleeps;
DROP TABLE IF EXISTS team_sleep_defaults;
ALTER TABLE apps
    DROP CONSTRAINT IF EXISTS apps_waking_since_check,
    DROP CONSTRAINT IF EXISTS apps_sleeping_since_check,
    DROP COLUMN IF EXISTS wake_failure,
    DROP COLUMN IF EXISTS wake_failed_at,
    DROP COLUMN IF EXISTS requests_seen_since,
    DROP COLUMN IF EXISTS request_count,
    DROP COLUMN IF EXISTS last_request_at,
    DROP COLUMN IF EXISTS awake_since,
    DROP COLUMN IF EXISTS waking_since,
    DROP COLUMN IF EXISTS sleeping_since,
    DROP COLUMN IF EXISTS sleep_state,
    DROP COLUMN IF EXISTS sleep_after_minutes,
    DROP COLUMN IF EXISTS sleep_mode;
-- +goose StatementEnd
