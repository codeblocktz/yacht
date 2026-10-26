-- +goose Up
-- Why an operator acted as a team: given when they start, kept with the
-- start, and read by the team in its audit log and by the operators in
-- Admin › Impersonations. Empty on a start recorded before it was asked, and
-- on every stop.
ALTER TABLE impersonation_events ADD COLUMN reason text NOT NULL DEFAULT '';

-- Admin › Impersonations reads every team's, newest first.
CREATE INDEX impersonation_events_at_idx ON impersonation_events (at DESC);

-- +goose Down
DROP INDEX IF EXISTS impersonation_events_at_idx;
ALTER TABLE impersonation_events DROP COLUMN IF EXISTS reason;
