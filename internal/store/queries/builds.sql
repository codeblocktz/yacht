-- Builds. Owner-scoped in the row rather than through the deployment, because
-- the log is readable output and the scoping has to be checkable in the query
-- that reads it.

-- name: CreateBuild :one
INSERT INTO builds (owner_id, app_id, deployment_id, repo_url, repo_ref)
VALUES (@owner_id, @app_id, @deployment_id, @repo_url, @repo_ref)
RETURNING *;

-- name: AppendBuildLog :exec
-- Appended rather than replaced, so a build streaming its output does not have
-- to hold the whole log in memory to write any of it.
--
-- Up to max_bytes, and then the marker once in place of the chunk that would
-- have crossed it, and nothing after. A build that prints without end would
-- otherwise grow one row without end, rewriting all of it on every line.
-- Decided in the statement, under the row lock, so two writers cannot both
-- fit a chunk the limit had room for only once, nor both write the marker.
UPDATE builds
SET log = CASE
        WHEN octet_length(log) + octet_length(@chunk::text) <= @max_bytes::bigint
            THEN log || @chunk::text
        ELSE log || @marker::text
    END,
    log_truncated = octet_length(log) + octet_length(@chunk::text) > @max_bytes::bigint
WHERE id = @id AND NOT log_truncated;

-- name: AppendBuildLogTail :exec
-- Output written after the build, the pre-deploy command's, into a reserve
-- beyond the build's own limit: max_bytes here is that limit and the
-- reserve together. A build that filled its log still shows what ran after
-- it, and a chunk the reserve has no room for is dropped, the command's
-- output being kept whole with its deployment besides.
UPDATE builds
SET log = log || @chunk::text
WHERE id = @id
  AND octet_length(log) + octet_length(@chunk::text) <= @max_bytes::bigint;

-- name: GetBuildLogFrom :one
-- A build log from a character offset, with where it ends and whether the
-- build still runs, for a reader following it: each poll sends what is new
-- rather than the whole log again.
SELECT CAST(substr(log, sqlc.arg(offset_chars)::int + 1) AS text) AS chunk,
       char_length(log)::int AS length,
       status
FROM builds
WHERE owner_id = @owner_id AND deployment_id = @deployment_id;

-- name: FinishBuild :one
-- A log cut at its limit ends with the marker and lost the build's last
-- lines, which are usually why it failed: the message, which says so, is
-- written after the marker. Returns the build without its log, which no
-- caller of this reads.
UPDATE builds
SET status      = @status,
    message     = @message,
    image       = @image,
    commit_sha  = @commit_sha,
    finished_at = now(),
    log = CASE
        WHEN log_truncated AND @message::text <> '' THEN log || @message::text || E'\n'
        ELSE log
    END
WHERE id = @id
RETURNING id, owner_id, app_id, deployment_id, repo_url, repo_ref, commit_sha,
          image, status, message, started_at, finished_at, job_name,
          build_args, log_truncated;

-- name: GetBuildForDeployment :one
-- The build with its log, for the page that shows the log.
SELECT * FROM builds
WHERE owner_id = @owner_id AND deployment_id = @deployment_id;

-- name: GetBuildMetaForDeployment :one
-- The build without its log, for everything that asks about a build rather
-- than reading what it printed. The same columns FinishBuild returns.
SELECT id, owner_id, app_id, deployment_id, repo_url, repo_ref, commit_sha,
       image, status, message, started_at, finished_at, job_name,
       build_args, log_truncated
FROM builds
WHERE owner_id = @owner_id AND deployment_id = @deployment_id;

-- name: GetBuild :one
SELECT * FROM builds
WHERE owner_id = @owner_id AND id = @id;

-- name: GetLatestSuccessfulBuildForApp :one
SELECT * FROM builds
WHERE owner_id = @owner_id AND app_id = @app_id
  AND status = 'succeeded' AND commit_sha <> ''
ORDER BY finished_at DESC NULLS LAST, started_at DESC, id DESC
LIMIT 1;

-- name: GetSuccessfulBuildForImage :one
-- The build that made an image, an upload's as well as a clone's: an upload
-- has no commit, and what it declared still decides whether a changed
-- variable is in the image. Read on every release, so without the log.
SELECT id, owner_id, app_id, deployment_id, repo_url, repo_ref, commit_sha,
       image, status, message, started_at, finished_at, job_name,
       build_args, log_truncated
FROM builds
WHERE owner_id = @owner_id AND app_id = @app_id
  AND status = 'succeeded' AND image = @image
ORDER BY finished_at DESC NULLS LAST, started_at DESC, id DESC
LIMIT 1;

-- name: SetBuildJob :exec
UPDATE builds SET job_name = @job_name WHERE id = @id;

-- name: ListRunningBuilds :many
-- Every build still claiming to run, oldest first. Not owner-scoped: this
-- feeds the reconciler, which is the platform settling its own records rather
-- than a team reading theirs. Without the log, which the reconciler never
-- reads and which would otherwise come along for every running build on
-- every pass.
SELECT id, owner_id, app_id, deployment_id, status, message,
       image, commit_sha, job_name, started_at
FROM builds WHERE status = 'running' ORDER BY started_at;

-- name: SetBuildArgs :exec
-- What the build's Dockerfile declared, recorded once it has pushed. See
-- migration 00074.
UPDATE builds SET build_args = @build_args::text[] WHERE id = @id;
