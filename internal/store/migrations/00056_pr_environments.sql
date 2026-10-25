-- +goose Up
-- +goose StatementBegin

-- A project can have an environment made for each pull request opened
-- against a repository it builds, and taken down when the pull request
-- closes, as Railway's PR Environments are. Off until somebody turns it on:
-- each one is every service of the project again.
ALTER TABLE projects ADD COLUMN pr_environments boolean NOT NULL DEFAULT false;

-- The pull request an environment was made for, so closing it finds it.
ALTER TABLE environments ADD COLUMN pr_number integer;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE environments DROP COLUMN IF EXISTS pr_number;
ALTER TABLE projects DROP COLUMN IF EXISTS pr_environments;
-- +goose StatementEnd
