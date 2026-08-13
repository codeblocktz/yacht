-- +goose Up
-- +goose StatementBegin

CREATE TABLE deployment_operations (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id       text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    app_id         uuid NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    deployment_id  uuid NOT NULL UNIQUE REFERENCES deployments (id) ON DELETE CASCADE,
    release_id     uuid REFERENCES app_releases (id) ON DELETE SET NULL,
    requires_build boolean NOT NULL DEFAULT false,
    status         text NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'claimed', 'succeeded', 'failed', 'cancelled')),
    message        text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    claimed_at     timestamptz,
    finished_at    timestamptz
);

-- This is the admission invariant, not an optimization. Two request handlers
-- racing on different database connections cannot both admit the same app.
CREATE UNIQUE INDEX deployment_operations_one_live_per_app
    ON deployment_operations (app_id)
    WHERE status IN ('queued', 'claimed');

CREATE INDEX deployment_operations_claim_idx
    ON deployment_operations (created_at, id) WHERE status = 'queued';
CREATE INDEX deployment_operations_owner_idx
    ON deployment_operations (owner_id, created_at DESC, id DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS deployment_operations;
-- +goose StatementEnd
