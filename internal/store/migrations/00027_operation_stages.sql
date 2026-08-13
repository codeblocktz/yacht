-- +goose Up
-- +goose StatementBegin

ALTER TABLE deployment_operations
    DROP CONSTRAINT deployment_operations_status_check,
    DROP CONSTRAINT deployment_operations_claim_shape;

ALTER TABLE deployment_operations
    ADD COLUMN checkpoint text NOT NULL DEFAULT 'claimed',
    ADD COLUMN stage_started_at timestamptz NOT NULL DEFAULT now(),
    ADD CONSTRAINT deployment_operations_status_check CHECK (
        status IN (
            'queued', 'claimed', 'building', 'applying', 'verifying',
            'succeeded', 'failed', 'cancelled'
        )
    ),
    ADD CONSTRAINT deployment_operations_checkpoint_check CHECK (
        checkpoint IN ('claimed', 'building', 'applying', 'verifying')
    ),
    ADD CONSTRAINT deployment_operations_claim_shape CHECK (
        (
            status IN ('claimed', 'building', 'applying', 'verifying')
            AND claim_token IS NOT NULL
            AND lease_expires_at IS NOT NULL
        ) OR (
            status NOT IN ('claimed', 'building', 'applying', 'verifying')
            AND claim_token IS NULL
            AND lease_expires_at IS NULL
        )
    );

DROP INDEX deployment_operations_one_live_per_app;
CREATE UNIQUE INDEX deployment_operations_one_live_per_app
    ON deployment_operations (app_id)
    WHERE status IN ('queued', 'claimed', 'building', 'applying', 'verifying');

DROP INDEX deployment_operations_expired_lease_idx;
CREATE INDEX deployment_operations_expired_lease_idx
    ON deployment_operations (lease_expires_at, id)
    WHERE status IN ('claimed', 'building', 'applying', 'verifying');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

UPDATE deployment_operations
SET status = 'queued', claimed_at = NULL,
    claim_token = NULL, lease_expires_at = NULL
WHERE status IN ('building', 'applying', 'verifying');

DROP INDEX deployment_operations_expired_lease_idx;
DROP INDEX deployment_operations_one_live_per_app;

ALTER TABLE deployment_operations
    DROP CONSTRAINT deployment_operations_claim_shape,
    DROP CONSTRAINT deployment_operations_checkpoint_check,
    DROP CONSTRAINT deployment_operations_status_check,
    DROP COLUMN stage_started_at,
    DROP COLUMN checkpoint;

ALTER TABLE deployment_operations
    ADD CONSTRAINT deployment_operations_status_check CHECK (
        status IN ('queued', 'claimed', 'succeeded', 'failed', 'cancelled')
    ),
    ADD CONSTRAINT deployment_operations_claim_shape CHECK (
        (status = 'claimed' AND claim_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR
        (status <> 'claimed' AND claim_token IS NULL AND lease_expires_at IS NULL)
    );

CREATE UNIQUE INDEX deployment_operations_one_live_per_app
    ON deployment_operations (app_id)
    WHERE status IN ('queued', 'claimed');
CREATE INDEX deployment_operations_expired_lease_idx
    ON deployment_operations (lease_expires_at, id)
    WHERE status = 'claimed';

-- +goose StatementEnd
