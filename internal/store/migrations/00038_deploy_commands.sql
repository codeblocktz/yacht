-- Deploy commands: what an app runs instead of its image's own command, and
-- what runs once, before each new version takes traffic.
--
-- Both empty by default, which is every app that exists: an empty start
-- command leaves the image's CMD and ENTRYPOINT as they are, and an empty
-- pre-deploy command runs nothing. Adding either to an app nobody asked to
-- change would roll it, or run something in it, for no reason.

-- +goose Up
-- +goose StatementBegin

-- Bounded rather than open, so a pasted script ends up in a review rather than
-- in a pod spec. Kubernetes would take far more; nobody types this much.
ALTER TABLE apps
    ADD COLUMN start_command text NOT NULL DEFAULT ''
        CHECK (length(start_command) <= 4096),
    ADD COLUMN pre_deploy_command text NOT NULL DEFAULT ''
        CHECK (length(pre_deploy_command) <= 4096);

-- In the release as well: both are part of what a deployment meant to run,
-- and a rollback runs the commands of the release it returns to, not the
-- ones the app has now. A migration written for the new schema run against
-- the old code is exactly the mistake a rollback must not make.
ALTER TABLE app_releases
    ADD COLUMN start_command text NOT NULL DEFAULT '',
    ADD COLUMN pre_deploy_command text NOT NULL DEFAULT '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE app_releases
    DROP COLUMN IF EXISTS pre_deploy_command,
    DROP COLUMN IF EXISTS start_command;
ALTER TABLE apps
    DROP COLUMN IF EXISTS pre_deploy_command,
    DROP COLUMN IF EXISTS start_command;
-- +goose StatementEnd
