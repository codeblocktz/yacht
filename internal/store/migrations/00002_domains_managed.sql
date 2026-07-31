-- +goose Up
-- +goose StatementBegin

-- A platform-issued hostname is marked by a column rather than recognised by
-- matching its suffix against the current app domain.
--
-- The app domain is configuration and can change. Suffix matching would give a
-- different answer after that change: rows issued under the old domain would
-- stop being recognised as platform-issued, and a customer's own domain could
-- start being recognised as one. A column records what was true at issue time
-- and does not move when configuration does.
ALTER TABLE domains ADD COLUMN managed boolean NOT NULL DEFAULT false;

-- An app has at most one platform-issued hostname. Partial, because the same
-- app may hold any number of custom domains, and a plain unique index on
-- app_id would forbid exactly what this table exists for.
CREATE UNIQUE INDEX domains_app_managed_key ON domains (app_id) WHERE managed;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS domains_app_managed_key;
ALTER TABLE domains DROP COLUMN IF EXISTS managed;
-- +goose StatementEnd
