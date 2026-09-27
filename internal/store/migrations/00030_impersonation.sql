-- An operator acting as a team, for support.
--
-- The hosted service built on the engine has people who run the install and
-- customers who have teams on it. When a customer needs help, the operator has
-- to see what the customer sees and fix what the customer cannot — which means
-- acting as their team, with every query scoped to it, rather than being handed
-- a membership that would outlive the conversation.
--
-- Acting is layered on the session rather than replacing its team. The
-- operator's own active team and membership stay exactly as they are, so
-- ending the impersonation is clearing two columns and nothing has to be put
-- back. And because it lives on one session, it ends with that session: signing
-- out, or the cookie expiring, cannot leave anybody acting.

-- +goose Up
-- +goose StatementBegin

-- Nullable, and SET NULL with the team: a team that is removed while somebody
-- is acting as it leaves the operator back in their own, not holding a session
-- that resolves to nothing.
--
-- Whether the person may still act is not stored here. It is checked on every
-- request, against the install's list of operators, so that removing somebody
-- from that list ends their impersonation on their next request rather than
-- whenever the session happens to expire.
ALTER TABLE sessions
    ADD COLUMN acting_team_id text REFERENCES teams (id) ON DELETE SET NULL,
    ADD COLUMN acting_since   timestamptz;

-- The durable record: who acted as which team, and when it started and
-- stopped. The log says the same thing, but a log is rotated, and "was anybody
-- from the install in our team last Tuesday" is a question a customer is
-- entitled to have answered months later.
--
-- owner_id is the team that was acted AS — that is whose record this is, and
-- what the team page reads it by. It goes with the team, like everything else
-- the team owns.
--
-- The operator is kept by id and by address. The id links to the person while
-- they exist; the address is what the record still says after they have been
-- removed, when the id would point at nobody and the row would stop saying who
-- it was about.
--
-- `at` is the clock rather than now(). now() is the transaction's start, and
-- moving straight from acting as one team to another writes a stop and a start
-- in one transaction — the record should say which came first.
CREATE TABLE impersonation_events (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id       text        NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_id        uuid        REFERENCES users (id) ON DELETE SET NULL,
    operator_email text        NOT NULL,
    action         text        NOT NULL CHECK (action IN ('start', 'stop')),
    at             timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX impersonation_events_owner_at_idx ON impersonation_events (owner_id, at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS impersonation_events;
ALTER TABLE sessions
    DROP COLUMN IF EXISTS acting_since,
    DROP COLUMN IF EXISTS acting_team_id;
-- +goose StatementEnd
