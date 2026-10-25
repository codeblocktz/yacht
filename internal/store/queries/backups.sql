-- Backups of a database, and restores into it. Scoped by owner_id wherever a
-- person asks, like every other resource query; the settler and the schedule
-- read across owners, because they act for every team.

-- name: CreateDatabaseBackup :one
-- Refused by database_backups_one_running when anything is already running
-- against this database, which is the point: the caller reports the unique
-- violation as "one at a time" rather than racing a second dump.
INSERT INTO database_backups (
    owner_id, app_id, kind, trigger, location, object_key, backup_id, created_by
)
VALUES (
    @owner_id, @app_id, @kind, @trigger, @location, @object_key, @backup_id, @created_by
)
RETURNING *;

-- name: SetDatabaseBackupJob :exec
-- The object and the Job are named after the row, so they are set once it
-- exists: the name is what lets a settler in any engine process find both.
UPDATE database_backups SET job_name = @job_name, object_key = @object_key
WHERE id = @id AND status = 'running';

-- name: FinishDatabaseBackup :execrows
-- Only from running, so two engine processes settling the same Job agree on
-- one outcome and the second changes nothing.
UPDATE database_backups
SET status = @status, size_bytes = @size_bytes, error = @error, finished_at = now()
WHERE id = @id AND status = 'running';

-- name: ListDatabaseBackups :many
SELECT * FROM database_backups
WHERE owner_id = @owner_id AND app_id = @app_id
ORDER BY started_at DESC, id DESC
LIMIT @result_limit;

-- name: GetDatabaseBackup :one
SELECT * FROM database_backups
WHERE owner_id = @owner_id AND app_id = @app_id AND id = @id;

-- name: DeleteDatabaseBackup :execrows
-- Never a running one: its Job is still writing, and the row is how the
-- settler finds it.
DELETE FROM database_backups
WHERE owner_id = @owner_id AND app_id = @app_id AND id = @id AND status <> 'running';

-- name: ListRunningDatabaseBackups :many
SELECT b.*, a.name AS app_name, a.namespace AS app_namespace
FROM database_backups b
JOIN apps a ON a.id = b.app_id
WHERE b.status = 'running'
ORDER BY b.started_at;

-- name: GetDatabaseBackupSchedule :one
SELECT * FROM database_backup_schedules
WHERE owner_id = @owner_id AND app_id = @app_id;

-- name: SetDatabaseBackupSchedule :exec
INSERT INTO database_backup_schedules (app_id, owner_id, daily)
VALUES (@app_id, @owner_id, @daily)
ON CONFLICT (app_id) DO UPDATE
SET daily = EXCLUDED.daily, updated_at = now();

-- name: ListDueDatabaseBackups :many
-- Every database whose daily backup is on and has not been taken, or tried,
-- in the last day — less a few minutes, so a schedule that ran at 03:00:05
-- yesterday still runs at about 03:00 today rather than drifting later.
SELECT a.owner_id, a.name
FROM database_backup_schedules s
JOIN apps a ON a.id = s.app_id
WHERE s.daily
  AND NOT EXISTS (
      SELECT 1 FROM database_backups b
      WHERE b.app_id = s.app_id
        AND (b.status = 'running'
             OR (b.kind = 'backup' AND b.trigger = 'scheduled'
                 AND b.started_at > now() - interval '23 hours 50 minutes'))
  )
ORDER BY a.owner_id, a.name;

-- name: ListExpiredScheduledBackups :many
-- The scheduled backups past the number kept, oldest last. Only succeeded
-- ones count toward the number: a failed night should not push a good backup
-- out.
SELECT b.* FROM database_backups b
WHERE b.owner_id = @owner_id AND b.app_id = @app_id
  AND b.kind = 'backup' AND b.trigger = 'scheduled' AND b.status = 'succeeded'
ORDER BY b.started_at DESC
OFFSET @keep;
