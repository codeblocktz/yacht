-- Customers' buckets (Add › Bucket): what Garage calls each one, so the
-- engine can measure it and delete it.

-- +goose Up
-- +goose StatementBegin

-- One bucket, and the service it is. The service's variables hold what an
-- app needs — BUCKET, ENDPOINT, ACCESS_KEY_ID, SECRET_ACCESS_KEY — and can be
-- edited, so what the engine itself relies on is kept here, where nobody
-- edits it: the secret too, sealed as a variable's is, for the Files tab.
--
-- size_bytes and objects are what Garage last said it held, measured every
-- few minutes; what usage and billing read.
CREATE TABLE buckets (
    app_id uuid PRIMARY KEY REFERENCES apps (id) ON DELETE CASCADE,
    owner_id text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    garage_id text NOT NULL UNIQUE,
    name text NOT NULL UNIQUE,
    key_id text NOT NULL,
    sealed_secret bytea NOT NULL,
    size_bytes bigint NOT NULL DEFAULT 0,
    objects bigint NOT NULL DEFAULT 0,
    measured_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS buckets;
-- +goose StatementEnd
