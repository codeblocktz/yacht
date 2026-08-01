-- The join settings are install-wide, so unlike every other query here these
-- take no owner_id. See the 00012 migration for why.

-- name: GetClusterJoin :one
SELECT * FROM cluster_join WHERE id = 1;

-- name: SetClusterJoin :one
INSERT INTO cluster_join (id, server_url, token_sealed, updated_by)
VALUES (1, @server_url, @token_sealed, @updated_by)
ON CONFLICT (id) DO UPDATE
    SET server_url   = EXCLUDED.server_url,
        token_sealed = EXCLUDED.token_sealed,
        updated_by   = EXCLUDED.updated_by,
        updated_at   = now()
RETURNING *;

-- name: ClearClusterJoin :execrows
DELETE FROM cluster_join WHERE id = 1;
