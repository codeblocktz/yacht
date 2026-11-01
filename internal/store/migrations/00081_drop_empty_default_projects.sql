-- +goose Up
-- A new team starts with no project and makes its own: reading the projects
-- page no longer creates "Default". Teams that already got one by looking —
-- empty, never renamed or set up — lose it, so they start clean as well.
--
-- Only a project nothing points at and nobody touched: slug default, still
-- named Default, pull-request environments off, no app in it (deleted apps
-- included, whose rows keep project_id until they are purged), and no
-- variable, environment, API token or webhook of its own.
DELETE FROM projects p
WHERE p.slug = 'default'
  AND p.name = 'Default'
  AND NOT p.pr_environments
  AND NOT EXISTS (SELECT 1 FROM apps a WHERE a.project_id = p.id)
  AND NOT EXISTS (SELECT 1 FROM deleted_apps d WHERE d.project_id = p.id)
  AND NOT EXISTS (SELECT 1 FROM project_variables v WHERE v.project_id = p.id)
  AND NOT EXISTS (SELECT 1 FROM environments e WHERE e.project_id = p.id)
  AND NOT EXISTS (SELECT 1 FROM api_tokens t WHERE t.project_id = p.id)
  AND NOT EXISTS (SELECT 1 FROM project_webhooks w WHERE w.project_id = p.id);

-- +goose Down
-- Nothing to restore: an empty default project is made again by the first
-- app created without one.
SELECT 1;
