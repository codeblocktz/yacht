-- Tokens a program holds instead of a session: a CLI on somebody's laptop, a
-- CI job deploying on merge. Railway's workspace and project tokens.

-- +goose Up
-- +goose StatementBegin

-- One credential for the API, belonging to a team.
--
-- Only the hash is kept, as with sessions and invitations: the token is shown
-- once when it is made, and a database dump is not a set of working tokens.
--
-- created_by is not decoration. A token is the team's, but it acts with no
-- more authority than the person who made it still has: resolving one joins
-- that person's membership, so removing them from the team stops their
-- tokens on the same request, and demoting an admin makes their admin tokens
-- act as members. Without that, leaving a team would be a way to keep a key
-- to it.
CREATE TABLE api_tokens (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,

    -- A project token reaches one project's apps and nothing else of the
    -- team's; null is a team token. Cascade, because a token for a project
    -- that no longer exists is a token for nothing.
    project_id uuid REFERENCES projects (id) ON DELETE CASCADE,

    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 60),

    -- Never owner: what only an owner may do — appointing admins, renaming
    -- the team — is not on the API, and a token that could would be one
    -- stolen laptop away from the team changing hands.
    role text NOT NULL CHECK (role IN ('member', 'admin')),

    token_hash bytea NOT NULL UNIQUE,

    -- The last characters of the token, so a list can say which one is which
    -- without holding anything that would let somebody use it.
    hint text NOT NULL DEFAULT '',

    -- Where it was made: on a settings page, or by signing a CLI in.
    origin text NOT NULL DEFAULT 'dashboard' CHECK (origin IN ('dashboard', 'cli')),

    created_by uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),

    -- Written at most once a minute per token, so a busy CI job does not
    -- turn every request into a write.
    last_used_at timestamptz,
    last_used_ip text NOT NULL DEFAULT '',

    -- Revoked rather than deleted, so the audit log's token names keep
    -- meaning something.
    revoked_at timestamptz
);

CREATE INDEX api_tokens_owner_idx ON api_tokens (owner_id, created_at DESC);

-- A CLI asking to be signed in through a browser.
--
-- The CLI holds the device code and polls with it; the person sees the user
-- code on the CLI's screen and on the approval page, and checks they are the
-- same before approving — the whole defence against somebody else's login
-- request being approved by a link they were sent. The device code is kept
-- hashed like every other bearer secret here.
--
-- No token is minted at approval. The poll that follows mints it and hands it
-- over once, so the raw token never has to be stored anywhere to wait.
CREATE TABLE cli_logins (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    device_hash bytea NOT NULL UNIQUE,
    user_code text NOT NULL UNIQUE,

    -- What the CLI says it is running on, shown on the approval page so the
    -- person can tell their laptop from a stranger's.
    client_name text NOT NULL DEFAULT '',

    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,

    approved_by uuid REFERENCES users (id) ON DELETE CASCADE,
    owner_id text REFERENCES teams (id) ON DELETE CASCADE,
    role text CHECK (role IN ('member', 'admin')),
    approved_at timestamptz,

    -- Spent: the token was handed to the poll, or the request was denied.
    consumed_at timestamptz,
    denied boolean NOT NULL DEFAULT false
);

CREATE INDEX cli_logins_expires_idx ON cli_logins (expires_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS cli_logins;
DROP TABLE IF EXISTS api_tokens;
-- +goose StatementEnd
