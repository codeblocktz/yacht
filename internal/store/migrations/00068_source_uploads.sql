-- Source uploaded by the CLI: `up` builds what is on somebody's disk rather
-- than what is on a branch.

-- +goose Up
-- +goose StatementBegin

-- One upload, and the deploy it is for.
--
-- In the database rather than on the engine's disk, because the build that
-- reads it may be started by any replica and fetches it from whichever one
-- the load balancer picks. The archive is bounded at upload, and dropped once
-- the build is over: it is what one build needed, not a history.
--
-- fetch_hash is the credential the build presents to download it, minted when
-- the build starts and good until fetch_expires_at. Hashed, like every other
-- bearer secret here; the build has the raw value in a Secret of its own.
CREATE TABLE source_uploads (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    app_id uuid NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    deployment_id uuid REFERENCES deployments (id) ON DELETE CASCADE,

    size_bytes bigint NOT NULL,
    sha256 text NOT NULL,
    archive bytea,

    fetch_hash bytea,
    fetch_expires_at timestamptz,
    fetched_at timestamptz,

    created_at timestamptz NOT NULL DEFAULT now(),
    -- When the archive was dropped; the row stays, so the deploy can still
    -- say how big what it built was.
    purged_at timestamptz
);

CREATE UNIQUE INDEX source_uploads_deployment_idx ON source_uploads (deployment_id);
CREATE INDEX source_uploads_unpurged_idx ON source_uploads (created_at) WHERE purged_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS source_uploads;
-- +goose StatementEnd
