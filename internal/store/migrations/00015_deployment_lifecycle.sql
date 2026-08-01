-- +goose Up
-- +goose StatementBegin

-- Two statuses the vocabulary was missing, and a backfill for the rows that
-- were written before anything closed them.
--
-- Every deployment sat at 'running' for ever: FinishDeployment existed and
-- nothing called it, so a history of finished deploys read as a list of things
-- still in progress. Nothing errored — the page was faithfully describing a
-- state that had not been true since the row was written.
--
-- 'active' is the one running now. 'superseded' is one it replaced, kept
-- distinct from 'failed' because a deployment that was replaced worked, and a
-- history of failures where none happened is worse than no history.
ALTER TABLE deployments DROP CONSTRAINT IF EXISTS deployments_status_check;
ALTER TABLE deployments ADD CONSTRAINT deployments_status_check
    CHECK (status IN (
        'pending', 'running', 'active', 'succeeded',
        'failed', 'cancelled', 'superseded'
    ));

-- The newest per app becomes active; everything older it replaced.
-- Ordered by started_at with id as the tie-break, because two rows written in
-- the same transaction share a timestamp and "newest" has to be decidable.
UPDATE deployments d
SET status      = 'superseded',
    finished_at = COALESCE(d.finished_at, d.started_at)
WHERE d.status IN ('running', 'pending')
  AND EXISTS (
      SELECT 1 FROM deployments newer
      WHERE newer.app_id = d.app_id
        AND (newer.started_at, newer.id) > (d.started_at, d.id)
  );

UPDATE deployments SET status = 'active'
WHERE status IN ('running', 'pending');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
UPDATE deployments SET status = 'running' WHERE status IN ('active', 'superseded');
ALTER TABLE deployments DROP CONSTRAINT IF EXISTS deployments_status_check;
ALTER TABLE deployments ADD CONSTRAINT deployments_status_check
    CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'cancelled'));
-- +goose StatementEnd
