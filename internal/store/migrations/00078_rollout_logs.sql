-- +goose Up
-- What a failed rollout's new version printed, kept with the deployment.
--
-- A redeploy that fails is rolled back, and its pod goes with the rollback,
-- taking the output that says why it failed: the deployment's Deploy logs
-- could then show only the old version's. Kept as it fails, the way the
-- pre-deploy command's output is, and in its own table for the same reason.
CREATE TABLE deployment_rollout_logs (
    deployment_id uuid        PRIMARY KEY REFERENCES deployments (id) ON DELETE CASCADE,
    owner_id      text        NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    pod           text        NOT NULL,
    previous      boolean     NOT NULL,
    output        text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX deployment_rollout_logs_owner_id_idx ON deployment_rollout_logs (owner_id);

-- +goose Down
DROP TABLE deployment_rollout_logs;
