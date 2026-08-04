-- +goose Up
-- +goose StatementBegin

-- Passwords are optional and additive.
--
-- A person enters through an invitation and a magic link; a password is
-- something they may later add so they can get in without waiting on mail. Both
-- routes end in the same session, so nothing downstream of sign-in can tell them
-- apart, and nothing downstream needs to.
--
-- Kept out of `users` deliberately. UpsertUser, GetUserByEmail, GetUserByID and
-- ConsumeMagicLink all SELECT *, so a column on `users` is a field on dbgen.User,
-- which is the argument of toUser, which is the type every handler already
-- holds. The hash would then be one %+v away from a log line, and nothing in the
-- type system would object. Its own table is what makes that impossible rather
-- than merely avoided — the same reasoning that keeps token_hash out of
-- ListPendingInvitations.
--
-- No owner_id. A password proves a person, not a tenant: the session it mints is
-- scoped to a team afterwards by active_team_id, and one human holding a
-- different password per team is not a thing anybody wants to support. users,
-- sessions and magic_links sit on this same side of that line.
CREATE TABLE user_credentials (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- Constrained to one value on purpose.
    --
    -- The table is named for credentials rather than for passwords because the
    -- shape — one row per person per method, holding one opaque string — is the
    -- right skeleton. The CHECK is what stops it becoming a junk drawer, and
    -- widening it later is one line; adding a discriminator to a table that
    -- already has live rows is not.
    --
    -- Two things must never be added to it. Passkeys are many per person and
    -- carry structured fields — credential id, public key, signature counter —
    -- so they get their own table. A TOTP shared secret must be REVERSIBLE,
    -- because you cannot compute a code from a hash, so it has to be sealed with
    -- internal/secret: the opposite storage rule to the one this column
    -- enforces. A column with two incompatible storage rules is a column that
    -- will eventually be read under the wrong one.
    kind       text        NOT NULL CHECK (kind IN ('password')),

    -- A versioned Argon2id hash in PHC encoding:
    --   $argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>
    --
    -- The parameters and a per-row random salt travel inside the string, so the
    -- cost this row was hashed at is a fact about the row rather than about
    -- whichever binary happens to be running. That is what makes raising the cost
    -- later a rehash on next login instead of a forced reset for everybody.
    --
    -- Named `secret` rather than `password_hash` so the one rule that matters
    -- reads the same at every call site: this column is never returned from the
    -- account package, in any type, under any name.
    secret     text        NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One password per person.
--
-- Without this, a replace that half-failed leaves two live rows, sign-in matches
-- whichever Postgres reaches first, and "remove my password" removes one of
-- them — leaving a credential nobody can see and nobody can delete.
CREATE UNIQUE INDEX user_credentials_kind_key ON user_credentials (user_id, kind);

-- When this session last proved who is holding it, as opposed to when it began.
--
-- Adding, changing or removing a password needs recent authentication, and
-- "recent" has to be a fact the server holds. The alternative — a short-lived
-- signed cookie, like the flash — was rejected: newFlashKey is random per process
-- on purpose, so a restart would silently un-prove everybody and two replicas
-- would never agree. Losing a toast to a restart is the correct thing to lose.
-- Losing an authorisation decision is not.
--
-- Separate from created_at because the two answer different questions. A session
-- that re-proves itself by re-entering its password is the same session, and
-- folding the two together would mean the only way to refresh the window is to
-- mint a new session — which is a sign-in, not a confirmation.
--
-- It costs no query. GetSessionByHash already reads s.*, so every request that
-- carries a cookie picks this up for free.
ALTER TABLE sessions ADD COLUMN authenticated_at timestamptz;

-- Backfilled from created_at rather than defaulted to now().
--
-- A plain DEFAULT now() would declare every session that happens to be open at
-- the moment this migration runs to be freshly authenticated, handing each of
-- them a free step-up window — including any an attacker is holding. What a
-- session actually proved, it proved when it was created.
UPDATE sessions SET authenticated_at = created_at;

ALTER TABLE sessions ALTER COLUMN authenticated_at SET NOT NULL;
ALTER TABLE sessions ALTER COLUMN authenticated_at SET DEFAULT now();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE sessions DROP COLUMN IF EXISTS authenticated_at;

DROP INDEX IF EXISTS user_credentials_kind_key;
DROP TABLE IF EXISTS user_credentials;

-- +goose StatementEnd
