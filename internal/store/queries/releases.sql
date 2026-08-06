-- Release snapshots are owner-scoped at every read and write, like every
-- other resource. There is intentionally no update query: a release is a fact
-- about a past deployment, not editable configuration.

-- name: CreateAppRelease :one
INSERT INTO app_releases (
    owner_id, app_id, image_ref, image_digest, source, source_revision,
    replicas, port, cpu_request, cpu_limit, memory_request, memory_limit,
    internal, run_as_user, fs_group, scratch_paths,
    writable_root_filesystem, health_path, health_liveness,
    env, secret_keys, config_version, origin
)
VALUES (
    @owner_id, @app_id, @image_ref, @image_digest, @source, @source_revision,
    @replicas, @port, @cpu_request, @cpu_limit, @memory_request, @memory_limit,
    @internal, @run_as_user, @fs_group, @scratch_paths,
    @writable_root_filesystem, @health_path, @health_liveness,
    @env, @secret_keys, @config_version, @origin
)
RETURNING *;

-- name: GetAppRelease :one
SELECT * FROM app_releases
WHERE owner_id = @owner_id AND app_id = @app_id AND id = @id;

-- name: ListAppReleases :many
SELECT * FROM app_releases
WHERE owner_id = @owner_id AND app_id = @app_id
ORDER BY created_at DESC, id DESC
LIMIT @result_limit;

-- name: SetDeploymentRelease :execrows
UPDATE deployments
SET release_id = @release_id
WHERE deployments.owner_id = @owner_id AND deployments.id = @deployment_id
  AND deployments.release_id IS NULL
  AND EXISTS (
      SELECT 1 FROM app_releases r
      WHERE r.id = @release_id AND r.owner_id = @owner_id
        AND r.app_id = deployments.app_id
  );

-- name: SetActiveRelease :execrows
UPDATE apps
SET active_release_id = @release_id, updated_at = now()
WHERE apps.owner_id = @owner_id AND apps.id = @app_id
  AND EXISTS (
      SELECT 1 FROM app_releases r
      WHERE r.id = @release_id AND r.owner_id = @owner_id
        AND r.app_id = @app_id
  );

-- name: ListPendingReleaseBackfills :many
SELECT sqlc.embed(a), b.state, b.last_error, b.attempts
FROM app_release_backfills b
JOIN apps a ON a.id = b.app_id AND a.owner_id = b.owner_id
WHERE b.state IN ('pending', 'image_unavailable', 'blocked')
  AND b.next_attempt_at <= now()
  AND a.active_release_id IS NULL
ORDER BY b.next_attempt_at, b.app_id
LIMIT @result_limit;

-- name: GetAppForReleaseBackfill :one
SELECT * FROM apps
WHERE owner_id = @owner_id AND id = @app_id
FOR UPDATE;

-- name: SetReleaseBackfillState :execrows
UPDATE app_release_backfills
SET state = @state, last_error = @last_error,
    attempts = attempts + 1,
    next_attempt_at = CASE
        WHEN @state = 'pending' THEN now() + interval '1 minute'
        WHEN @state IN ('image_unavailable', 'blocked')
            THEN now() + interval '15 minutes'
        ELSE now()
    END,
    updated_at = now()
WHERE owner_id = @owner_id AND app_id = @app_id;

-- name: GetReleaseBackfillState :one
SELECT * FROM app_release_backfills
WHERE owner_id = @owner_id AND app_id = @app_id;

-- name: CountPendingReleaseBackfills :one
SELECT count(*) FROM app_release_backfills WHERE state = 'pending';

-- name: NormalizeLegacyDeploymentStatuses :execrows
UPDATE deployments
SET status = 'succeeded', finished_at = COALESCE(finished_at, started_at)
WHERE status IN ('active', 'superseded');

-- name: IncrementAppConfigVersion :one
UPDATE apps
SET config_version = config_version + 1, updated_at = now()
WHERE owner_id = @owner_id AND id = @app_id
RETURNING config_version;
