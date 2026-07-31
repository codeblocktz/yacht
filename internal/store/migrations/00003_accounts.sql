-- +goose Up
-- +goose StatementBegin

-- A team IS an owner. Renaming rather than adding a table is what makes this
-- change cheap: Postgres carries foreign keys, indexes and constraints through
-- a rename, so every existing owner_id reference follows automatically and no
-- query in apps, deployments or domains changes at all.
--
-- This is the return on putting owner_id on every table in the first place.
-- Had ownership been a join through two other tables, this would be a rewrite.
ALTER TABLE owners RENAME TO teams;

-- Column names stay owner_id. Renaming them to team_id would touch every query
-- and generated struct for no behavioural gain.

-- A person, independent of any team.
CREATE TABLE users (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email        text        NOT NULL,
    display_name text        NOT NULL DEFAULT '',

    -- Carried now, unused until 2FA lands. Adding a column later is a
    -- migration across live data; two nullable ones cost nothing today.
    totp_secret    text,
    totp_confirmed boolean   NOT NULL DEFAULT false,

    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Addresses are compared case-insensitively: Alice@ and alice@ are one person,
-- and treating them as two is how an invitation goes to an account nobody
-- can sign in to.
CREATE UNIQUE INDEX users_email_key ON users (lower(email));

-- Where roles live. A person may hold one role in a team, not two.
CREATE TABLE memberships (
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    owner_id   text        NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    role       text        NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, owner_id)
);

CREATE INDEX memberships_owner_id_idx ON memberships (owner_id);

-- Sessions store a hash, never a token. A database dump must not be a set of
-- working credentials.
CREATE TABLE sessions (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash     bytea       NOT NULL,
    active_team_id text        REFERENCES teams (id) ON DELETE SET NULL,
    user_agent     text        NOT NULL DEFAULT '',
    ip             text        NOT NULL DEFAULT '',
    expires_at     timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX sessions_token_hash_key ON sessions (token_hash);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- Invitations are team-scoped, so they carry owner_id and stay under the
-- owner-scoping invariant.
CREATE TABLE invitations (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id    text        NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    email       text        NOT NULL,
    role        text        NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    token_hash  bytea       NOT NULL,
    invited_by  uuid        REFERENCES users (id) ON DELETE SET NULL,
    expires_at  timestamptz NOT NULL,
    accepted_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX invitations_token_hash_key ON invitations (token_hash);
CREATE INDEX invitations_owner_id_idx ON invitations (owner_id);

-- One outstanding invitation per address per team. Without this, resending
-- creates a second live token and revoking the first appears to do nothing.
CREATE UNIQUE INDEX invitations_pending_key
    ON invitations (owner_id, lower(email)) WHERE accepted_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS invitations;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS users;
ALTER TABLE teams RENAME TO owners;
-- +goose StatementEnd
