-- Install-wide DNS settings. No owner_id, for the reason cluster_join has
-- none: they configure one controller shared by every team.

-- name: GetPlatformDNS :one
SELECT * FROM platform_dns WHERE id = 1;

-- name: SetPlatformDNS :one
INSERT INTO platform_dns (id, cname_target, txt_prefix)
VALUES (1, @cname_target, @txt_prefix)
ON CONFLICT (id) DO UPDATE
    SET cname_target = EXCLUDED.cname_target,
        txt_prefix   = EXCLUDED.txt_prefix,
        updated_at   = now()
RETURNING *;
