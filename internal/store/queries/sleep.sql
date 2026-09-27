-- Apps that sleep when idle. See the 00033 migration.

-- name: GetTeamSleepDefault :one
SELECT * FROM team_sleep_defaults WHERE owner_id = @owner_id;

-- name: UpsertTeamSleepDefault :one
INSERT INTO team_sleep_defaults (owner_id, enabled, after_minutes)
VALUES (@owner_id, @enabled, @after_minutes)
ON CONFLICT (owner_id) DO UPDATE
SET enabled       = excluded.enabled,
    after_minutes = excluded.after_minutes,
    updated_at    = now()
RETURNING *;

-- The idle clock restarts with the setting, so turning sleep on for an app
-- that has been quiet all week does not put it to sleep a minute later.
-- name: SetAppSleepSetting :one
UPDATE apps
SET sleep_mode          = @sleep_mode,
    sleep_after_minutes = sqlc.narg('sleep_after_minutes'),
    awake_since         = greatest(awake_since, now())
WHERE owner_id = @owner_id AND id = @id
RETURNING *;

-- Awake apps that sleep, by their own setting or their team's. Every team's:
-- the sleeper runs for the whole install, like the reconciler.
-- name: ListSleepCandidates :many
SELECT sqlc.embed(a),
       coalesce(d.enabled, false)::boolean      AS team_enabled,
       coalesce(d.after_minutes, 30)::integer   AS team_after_minutes
FROM apps a
LEFT JOIN team_sleep_defaults d ON d.owner_id = a.owner_id
WHERE a.sleep_state = 'awake'
  AND a.active_release_id IS NOT NULL
  AND (a.sleep_mode = 'on' OR (a.sleep_mode = 'inherit' AND coalesce(d.enabled, false)))
ORDER BY a.id;

-- Records the ingress controller's counter for an app. A counter that moved
-- in either direction is a request: up is traffic, and down is the controller
-- having restarted, which is not a reason to believe nobody came.
-- name: RecordRequestCount :one
UPDATE apps
SET request_count       = @request_count::bigint,
    requests_seen_since = coalesce(requests_seen_since, now()),
    last_request_at     = CASE
        WHEN request_count IS NOT NULL AND request_count <> @request_count::bigint THEN now()
        ELSE last_request_at
    END
WHERE owner_id = @owner_id AND id = @id
RETURNING *;

-- A request the waker answered, for an app the counter cannot see because
-- its route is the waker's.
-- name: TouchAppRequest :exec
UPDATE apps SET last_request_at = now()
WHERE owner_id = @owner_id AND id = @id;

-- name: MarkAppAsleep :one
UPDATE apps
SET sleep_state    = 'asleep',
    sleeping_since = now(),
    waking_since   = NULL,
    wake_failed_at = NULL,
    wake_failure   = '',
    config_version = config_version + 1
WHERE owner_id = @owner_id AND id = @id AND sleep_state = 'awake'
RETURNING *;

-- name: OpenAppSleep :exec
INSERT INTO app_sleeps (owner_id, app_id, slept_at, idle_minutes)
VALUES (@owner_id, @app_id, @slept_at, sqlc.narg('idle_minutes'));

-- Guarded on 'asleep', so of every request, button and replica trying to wake
-- one app at once exactly one does.
-- name: MarkAppWaking :one
UPDATE apps
SET sleep_state    = 'waking',
    waking_since   = now(),
    config_version = config_version + 1
WHERE owner_id = @owner_id AND id = @id AND sleep_state = 'asleep'
RETURNING *;

-- From whichever of the states the caller names: a finished wake from
-- 'waking', a deploy from either.
-- name: MarkAppAwake :one
UPDATE apps
SET sleep_state    = 'awake',
    sleeping_since = NULL,
    waking_since   = NULL,
    awake_since    = now(),
    wake_failed_at = NULL,
    wake_failure   = '',
    config_version = config_version + 1
WHERE owner_id = @owner_id AND id = @id AND sleep_state = ANY(@from_states::text[])
RETURNING *;

-- name: CloseAppSleep :exec
UPDATE app_sleeps
SET woke_at = greatest(@woke_at::timestamptz, slept_at), woke_by = @woke_by::text
WHERE owner_id = @owner_id AND app_id = @app_id AND woke_at IS NULL;

-- A wake that started and did not finish puts the app back to sleep. The
-- interval it would have closed stays open: it never came up to serve anyone.
-- name: MarkAppWakeFailed :one
UPDATE apps
SET sleep_state    = 'asleep',
    waking_since   = NULL,
    wake_failed_at = now(),
    wake_failure   = @wake_failure::text,
    config_version = config_version + 1
WHERE owner_id = @owner_id AND id = @id AND sleep_state = 'waking'
RETURNING *;

-- A wake refused before it started: nothing was asked of the cluster, so the
-- workload is as it was and only the reason changes.
-- name: MarkAppWakeRefused :exec
UPDATE apps
SET wake_failed_at = now(), wake_failure = 'no-room'
WHERE owner_id = @owner_id AND id = @id AND sleep_state = 'asleep';

-- The app a hostname routes to. Not owner-scoped, and it cannot be: the waker
-- is reached by a request for a hostname, and the hostname is the whole of
-- what it knows. It returns only what the ingress controller would have
-- routed there anyway — a managed name, or a custom one that was proven.
-- name: GetAppByRoutableHost :one
SELECT a.* FROM apps a
JOIN domains d ON d.app_id = a.id
WHERE d.host = lower(@host) AND (d.managed OR d.verified)
LIMIT 1;

-- Wakes whose process went away: a restart, or a replica that stopped.
-- name: ListStaleWakes :many
SELECT * FROM apps
WHERE sleep_state = 'waking' AND waking_since < @before::timestamptz
ORDER BY id;

-- name: ListSleepingApps :many
SELECT * FROM apps
WHERE sleep_state <> 'awake'
ORDER BY id;

-- An app's own history, newest first.
-- name: ListAppSleeps :many
SELECT * FROM app_sleeps
WHERE owner_id = @owner_id AND app_id = @app_id
ORDER BY slept_at DESC
LIMIT @max_rows;

-- Every interval of a team's that overlaps [since, until), with the app's
-- name as it is now.
-- name: ListSleepIntervals :many
SELECT s.id, s.app_id, a.name AS app_name, s.slept_at, s.idle_minutes, s.woke_at, s.woke_by
FROM app_sleeps s
JOIN apps a ON a.id = s.app_id
WHERE s.owner_id = @owner_id
  AND s.slept_at < @until::timestamptz
  AND (s.woke_at IS NULL OR s.woke_at > @since::timestamptz)
ORDER BY s.slept_at;

-- A deploy restarts the idle clock: somebody who has just shipped a change is
-- about to go and look at it.
-- name: ResetIdleClock :exec
UPDATE apps SET awake_since = now()
WHERE owner_id = @owner_id AND id = @id;
