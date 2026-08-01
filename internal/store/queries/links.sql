-- name: ReplaceAppLinks :exec
DELETE FROM app_links WHERE from_app_id = @from_app_id AND via_key = @via_key;

-- name: CreateAppLink :exec
INSERT INTO app_links (owner_id, from_app_id, to_app_id, via_key)
VALUES (@owner_id, @from_app_id, @to_app_id, @via_key)
ON CONFLICT DO NOTHING;

-- name: ListAppLinks :many
SELECT l.from_app_id, l.to_app_id, l.via_key,
       f.name AS from_name, t.name AS to_name
FROM app_links l
JOIN apps f ON f.id = l.from_app_id
JOIN apps t ON t.id = l.to_app_id
WHERE l.owner_id = @owner_id
ORDER BY f.name, t.name, l.via_key;
