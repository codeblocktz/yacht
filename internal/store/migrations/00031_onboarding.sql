-- Whether a team has put the getting-started checklist away.
--
-- The checklist itself is not stored: every step is computed from what the
-- team actually has — apps, a deploy that succeeded, a routed custom domain, a
-- second member — so it cannot disagree with the dashboard around it. The one
-- thing that is a choice rather than a fact is somebody saying "I know, stop
-- showing me this", and that is all this records.
--
-- A column on teams rather than a table of its own, and on the server rather
-- than in the browser: the checklist belongs to the team, not to one laptop.
-- Dismissed on one machine it stays dismissed on the next, and for the
-- teammate who would otherwise be walked through steps the team took a month
-- ago. Nullable because most teams have never dismissed it; the timestamp is
-- the flag.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE teams ADD COLUMN onboarding_dismissed_at timestamptz;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE teams DROP COLUMN IF EXISTS onboarding_dismissed_at;
-- +goose StatementEnd
