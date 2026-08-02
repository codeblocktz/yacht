-- +goose Up
-- +goose StatementBegin

-- The Kubernetes Job that ran this build.
--
-- Recorded so a build can be reconciled against the cluster rather than
-- against the process that started it. A goroutine is not a source of truth:
-- it does not survive a restart, and it does not exist at all on the other
-- replicas. The Job does, which is what makes settling an interrupted build a
-- lookup instead of a guess.
ALTER TABLE builds ADD COLUMN job_name text NOT NULL DEFAULT '';

-- Finding the ones that need settling. Partial, because the query only ever
-- asks about running builds and they are the small minority.
CREATE INDEX builds_running_idx ON builds (started_at) WHERE status = 'running';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS builds_running_idx;
ALTER TABLE builds DROP COLUMN IF EXISTS job_name;
-- +goose StatementEnd
