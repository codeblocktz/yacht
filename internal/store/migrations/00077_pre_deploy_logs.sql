-- +goose Up
-- What a deployment's pre-deploy command printed, kept with the deployment.
--
-- The command runs in a Job deleted once it finishes, so its pod's log goes
-- with it, and a deploy that built nothing has no build log to add it to:
-- the output reached only a log store, if the install had one, and never
-- the deployment's own Deploy logs. Its own table rather than a column on
-- deployments, which every history list reads whole.
CREATE TABLE deployment_pre_deploy_logs (
    deployment_id uuid        PRIMARY KEY REFERENCES deployments (id) ON DELETE CASCADE,
    owner_id      text        NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    output        text        NOT NULL,
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX deployment_pre_deploy_logs_owner_id_idx ON deployment_pre_deploy_logs (owner_id);

-- +goose Down
DROP TABLE deployment_pre_deploy_logs;
