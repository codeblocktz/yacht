-- +goose Up
-- A database update that waits on a backup.
--
-- Updating a database takes a backup first, and the backup is a Job that
-- takes minutes. The update it waits for is kept on the backup's own row,
-- so whichever engine process settles the backup deploys it, and an engine
-- restarted in between still does: nothing is held in memory across it.
ALTER TABLE database_backups DROP CONSTRAINT database_backups_trigger_check;
ALTER TABLE database_backups ADD CONSTRAINT database_backups_trigger_check
    CHECK (trigger IN ('manual', 'scheduled', 'update'));

-- The image the update deploys once the backup succeeds, and what it is
-- called in the deployments list: "PostgreSQL 16.16". Empty for every
-- backup that is not taken for an update.
ALTER TABLE database_backups
    ADD COLUMN update_image text NOT NULL DEFAULT '',
    ADD COLUMN update_label text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE database_backups DROP COLUMN update_image, DROP COLUMN update_label;
DELETE FROM database_backups WHERE trigger = 'update';
ALTER TABLE database_backups DROP CONSTRAINT database_backups_trigger_check;
ALTER TABLE database_backups ADD CONSTRAINT database_backups_trigger_check
    CHECK (trigger IN ('manual', 'scheduled'));
