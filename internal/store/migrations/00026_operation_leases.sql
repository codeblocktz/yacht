-- +goose Up
-- +goose StatementBegin

ALTER TABLE deployment_operations
    ADD COLUMN claim_token uuid,
    ADD COLUMN lease_expires_at timestamptz,
    ADD COLUMN cancelled_at timestamptz;

ALTER TABLE deployment_operations
    ADD CONSTRAINT deployment_operations_claim_shape CHECK (
        (status = 'claimed' AND claim_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR
        (status <> 'claimed' AND claim_token IS NULL AND lease_expires_at IS NULL)
    );

CREATE INDEX deployment_operations_expired_lease_idx
    ON deployment_operations (lease_expires_at, id)
    WHERE status = 'claimed';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS deployment_operations_expired_lease_idx;
ALTER TABLE deployment_operations
    DROP CONSTRAINT IF EXISTS deployment_operations_claim_shape,
    DROP COLUMN IF EXISTS cancelled_at,
    DROP COLUMN IF EXISTS lease_expires_at,
    DROP COLUMN IF EXISTS claim_token;
-- +goose StatementEnd
