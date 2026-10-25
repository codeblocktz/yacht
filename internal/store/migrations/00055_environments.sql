-- +goose Up
-- +goose StatementBegin

-- An environment is a copy of a project's services, as Railway's are:
-- production, and as many others — staging, a pull request's — as somebody
-- makes. Each holds its own apps, with their own variables, domains and
-- deployments.
--
-- Production is implicit: every project has it and no row says so. A row is
-- only the environments made beside it, so a project nobody has given a
-- second environment reads exactly as before.
CREATE TABLE environments (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id   text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,

    slug       text NOT NULL CHECK (slug ~ '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$' AND length(slug) <= 30
                                    AND slug <> 'production'),
    name       text NOT NULL CHECK (length(name) BETWEEN 1 AND 60),

    -- Where it was copied from, for the settings page to say; empty for one
    -- made empty.
    copied_from text NOT NULL DEFAULT '',

    created_at timestamptz NOT NULL DEFAULT now(),

    UNIQUE (project_id, slug)
);

CREATE INDEX environments_owner_idx ON environments (owner_id);

-- An app is one service in one environment. The name stays what addresses
-- it — URLs, namespaces, in-cluster hosts are all made from it and do not
-- change — and the service is what the canvas calls it: web in production is
-- the app web, web in staging the app web-staging, and both are "web".
ALTER TABLE apps ADD COLUMN environment text NOT NULL DEFAULT 'production';
ALTER TABLE apps ADD COLUMN service text NOT NULL DEFAULT '';
UPDATE apps SET service = name WHERE service = '';

-- One of each service per environment of a project. Apps with no project yet
-- are adopted into one on the next read and are not counted until then.
CREATE UNIQUE INDEX apps_project_environment_service_key
    ON apps (project_id, environment, service) WHERE project_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS apps_project_environment_service_key;
ALTER TABLE apps DROP COLUMN IF EXISTS service;
ALTER TABLE apps DROP COLUMN IF EXISTS environment;
DROP TABLE IF EXISTS environments;
-- +goose StatementEnd
