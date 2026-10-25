-- Who did what to a team, when, and from where: Railway's workspace Audit
-- Logs.

-- +goose Up
-- +goose StatementBegin

-- One thing somebody did.
--
-- Separate from deployments, which say what happened to an app, and from the
-- application log, which says it to whoever runs the install and is gone at
-- the next rotation. This is the team's own record, kept, and readable by its
-- admins.
--
-- What must never be written here is the point of half the columns' comments:
-- a variable's key and never its value, that a secret was revealed and never
-- what it was, that a console session began and never what was typed in it.
-- detail is a short map of names chosen in code, not a copy of the request.
CREATE TABLE audit_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    at timestamptz NOT NULL DEFAULT now(),

    -- Who: a person, a token, or the platform itself. actor_id is the user's
    -- or the token's id; actor_name is what the page shows — an address, a
    -- token's name — kept as it was when they acted, so renaming or
    -- revoking later does not rewrite history.
    actor_kind text NOT NULL CHECK (actor_kind IN ('user', 'token', 'operator', 'system')),
    actor_id text NOT NULL DEFAULT '',
    actor_name text NOT NULL DEFAULT '',

    -- From where.
    ip text NOT NULL DEFAULT '',
    user_agent text NOT NULL DEFAULT '',

    -- What, as a dotted name: deploy.redeploy, variable.set, member.remove.
    action text NOT NULL,

    -- To what: an app, a project, a member, a token, a domain.
    target_kind text NOT NULL DEFAULT '',
    target text NOT NULL DEFAULT '',

    detail jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX audit_events_owner_at_idx ON audit_events (owner_id, at DESC);
CREATE INDEX audit_events_owner_action_idx ON audit_events (owner_id, action, at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS audit_events;
-- +goose StatementEnd
