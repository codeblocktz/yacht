-- Source the CLI uploaded for one build. Owner-scoped like the build it is
-- for; the fetch is the one read that is not, because a build pod has no
-- team — it has the fetch token, which is checked instead.

-- name: CreateSourceUpload :one
INSERT INTO source_uploads (owner_id, app_id, size_bytes, sha256, archive)
VALUES (@owner_id, @app_id, @size_bytes, @sha256, @archive)
RETURNING id, owner_id, app_id, deployment_id, size_bytes, sha256, created_at;

-- name: AttachSourceUpload :execrows
UPDATE source_uploads SET deployment_id = @deployment_id
WHERE owner_id = @owner_id AND id = @id AND deployment_id IS NULL;

-- name: GetSourceUploadForDeployment :one
SELECT id, owner_id, app_id, deployment_id, size_bytes, sha256, created_at, purged_at
FROM source_uploads
WHERE owner_id = @owner_id AND deployment_id = @deployment_id;

-- name: SetSourceUploadFetch :execrows
UPDATE source_uploads
SET fetch_hash = @fetch_hash, fetch_expires_at = @fetch_expires_at
WHERE owner_id = @owner_id AND id = @id AND purged_at IS NULL;

-- name: GetSourceUploadArchive :one
SELECT id, fetch_hash, fetch_expires_at, archive, sha256
FROM source_uploads
WHERE id = @id AND purged_at IS NULL;

-- name: MarkSourceUploadFetched :exec
UPDATE source_uploads SET fetched_at = now() WHERE id = @id;

-- name: PurgeSourceUpload :exec
UPDATE source_uploads
SET archive = NULL, fetch_hash = NULL, purged_at = now()
WHERE owner_id = @owner_id AND id = @id AND purged_at IS NULL;

-- Archives nobody will build any more: never attached to a deploy, or kept
-- past any build that could still want them.
-- name: PurgeStaleSourceUploads :execrows
UPDATE source_uploads
SET archive = NULL, fetch_hash = NULL, purged_at = now()
WHERE purged_at IS NULL AND created_at < @before;
