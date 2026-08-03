-- +goose Up
-- +goose StatementBegin

-- A domain's state, rather than whether somebody once proved it.
--
-- The boolean this replaces could only say "proven" or "not proven", which is
-- one bit for a question with several answers. A name that does not resolve at
-- all and a name that resolves to somebody else's load balancer were the same
-- value, and both rendered as "not verified" — so the one piece of information
-- that would have told the person what to fix was the piece never recorded.
--
-- Nothing ever cleared the boolean either. A domain whose DNS was deleted stayed
-- proven forever, and stayed in the Ingress with it.
ALTER TABLE domains ADD COLUMN state text NOT NULL DEFAULT 'pending';

-- Everything already routed keeps routing. A managed host is routed because the
-- platform issued it; a custom one because it was proven against a target that
-- has not changed since. Anything else starts over at pending, and the checker
-- settles it within seconds of this migration running — which is cheaper than
-- guessing here at a state nothing has observed.
UPDATE domains SET state = 'routed' WHERE managed OR verified;

ALTER TABLE domains ADD CONSTRAINT domains_state_check CHECK (
    state IN ('pending', 'awaiting_dns', 'misdirected', 'verified', 'routed', 'drifted')
);

-- What the name resolves to right now.
--
-- The reason the state column is worth having. "Points elsewhere" is not
-- something anybody can act on; "points at ghs.googlehosted.com" is the sentence
-- that ends the support conversation.
ALTER TABLE domains ADD COLUMN observed text NOT NULL DEFAULT '';

-- Why the last check could not answer, as opposed to what it found. A resolver
-- timing out is not evidence that a domain is wrong, and the two must not be
-- recorded in the same place or they will be read as the same thing.
ALTER TABLE domains ADD COLUMN last_error text NOT NULL DEFAULT '';

ALTER TABLE domains ADD COLUMN last_checked_at timestamptz;

-- When this row is next due. The schedule lives on the row rather than in the
-- checker's head so it survives a restart, and so several replicas can share the
-- work without agreeing on anything but the clock.
ALTER TABLE domains ADD COLUMN next_check_at timestamptz NOT NULL DEFAULT now();

-- Drives the backoff. Somebody who just created a record is watching; a domain
-- that has been live for a month is not.
ALTER TABLE domains ADD COLUMN check_attempts integer NOT NULL DEFAULT 0;

-- verified stops being a column anybody writes.
--
-- RoutableHostsForApp gates the Ingress on it, and that gate lives in SQL
-- precisely so no caller can forget it. Deriving the column from state extends
-- the same idea one step further: the routing gate and the state shown on screen
-- are now the same fact, so they cannot drift apart the way a hand-maintained
-- pair eventually does.
--
-- Spelled NOT NULL even though the expression cannot produce one: state is NOT
-- NULL and IN over a non-null operand always answers. Saying so is what keeps
-- the column a plain bool for anything reading the schema, rather than a
-- nullable one every caller has to dereference.
ALTER TABLE domains DROP COLUMN verified;
ALTER TABLE domains ADD COLUMN verified boolean NOT NULL
    GENERATED ALWAYS AS (state IN ('verified', 'routed')) STORED;

-- The checker's claim query, which reads exactly this shape every few seconds.
-- Partial, because managed rows are never checked and would otherwise be most of
-- the index on a healthy install.
CREATE INDEX domains_due_idx ON domains (next_check_at)
    WHERE NOT managed;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS domains_due_idx;

-- verified goes back to being a column that is written, carrying forward what
-- state had settled on. Rolling back must not un-route a live domain.
ALTER TABLE domains DROP COLUMN IF EXISTS verified;
ALTER TABLE domains ADD COLUMN verified boolean NOT NULL DEFAULT false;
UPDATE domains SET verified = true WHERE state IN ('verified', 'routed');

ALTER TABLE domains DROP CONSTRAINT IF EXISTS domains_state_check;
ALTER TABLE domains DROP COLUMN IF EXISTS check_attempts;
ALTER TABLE domains DROP COLUMN IF EXISTS next_check_at;
ALTER TABLE domains DROP COLUMN IF EXISTS last_checked_at;
ALTER TABLE domains DROP COLUMN IF EXISTS last_error;
ALTER TABLE domains DROP COLUMN IF EXISTS observed;
ALTER TABLE domains DROP COLUMN IF EXISTS state;

-- +goose StatementEnd
