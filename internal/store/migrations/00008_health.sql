-- +goose Up
-- +goose StatementBegin

-- An HTTP path on the app's own port that reports whether it is serving.
--
-- Empty means no probe at all, which is the behaviour every existing app has
-- and keeps: adding a probe to a workload nobody asked to probe would start
-- withholding traffic from apps that were fine.
ALTER TABLE apps ADD COLUMN health_path text NOT NULL DEFAULT ''
    CHECK (health_path = '' OR health_path LIKE '/%');

-- Whether the same path may also restart the container.
--
-- Separate from the path, and defaulting to off, because the two do very
-- different things. Readiness withholds traffic; liveness kills the process.
-- One switch for both would hand every app the second as a side effect of
-- wanting the first.
ALTER TABLE apps ADD COLUMN health_liveness boolean NOT NULL DEFAULT false;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE apps DROP COLUMN IF EXISTS health_liveness;
ALTER TABLE apps DROP COLUMN IF EXISTS health_path;
-- +goose StatementEnd
