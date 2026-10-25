-- Project webhooks: Railway's Project Settings › Webhooks.

-- +goose Up
-- +goose StatementBegin

-- Somewhere a project's events are sent.
--
-- The URL is stored as it is — it is shown on the page, and Discord's and
-- Slack's carry their own token in the path, which is why only admins see the
-- page at all. The custom headers are a different matter: an Authorization
-- header is the usual reason to set one, so they are sealed with the
-- install's key and never shown again, only their names. The signing secret
-- is sealed for the same reason and shown once, when the webhook is made.
CREATE TABLE project_webhooks (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,

    url text NOT NULL CHECK (char_length(url) BETWEEN 1 AND 2048),

    -- deploy.succeeded, deploy.failed, deploy.crashed, deploy.removed,
    -- app.crashed. Checked in code rather than here, so adding one is not a
    -- migration.
    events text[] NOT NULL DEFAULT '{}',

    -- The names of the custom headers, readable, and the whole set — names
    -- and values — sealed. Empty when there are none.
    header_names text[] NOT NULL DEFAULT '{}',
    headers_sealed bytea,

    secret_sealed bytea NOT NULL,

    created_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX project_webhooks_project_idx ON project_webhooks (owner_id, project_id);

-- One attempt to tell a webhook something, and every retry of it.
--
-- A row per event per webhook, written when the event happens, so a delivery
-- survives the process that would have sent it and any replica can send it.
-- The payload is kept so the deliveries log can say what was sent; it never
-- carries a variable's value or a custom header, because nothing that builds
-- one has either.
CREATE TABLE webhook_deliveries (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    webhook_id uuid NOT NULL REFERENCES project_webhooks (id) ON DELETE CASCADE,
    owner_id text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,

    event text NOT NULL,
    payload jsonb NOT NULL,

    -- pending until it is answered with a 2xx or given up on.
    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'succeeded', 'failed')),
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),

    -- The last answer: a status code, or why there was none. The response
    -- body is kept short, because it is somebody else's server talking.
    last_status integer,
    last_error text NOT NULL DEFAULT '',
    last_response text NOT NULL DEFAULT '',
    last_duration_ms integer,

    -- Held while one process is sending it, so two replicas do not.
    locked_until timestamptz,

    created_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);

CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at)
    WHERE status = 'pending';
CREATE INDEX webhook_deliveries_webhook_idx ON webhook_deliveries (webhook_id, created_at DESC);

-- Whether an app was failing when last looked at, so "app crashed" is said
-- once when it starts failing rather than on every look. Shared by replicas:
-- the one that flips it is the one that sends.
CREATE TABLE webhook_app_health (
    app_id uuid PRIMARY KEY REFERENCES apps (id) ON DELETE CASCADE,
    owner_id text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    failing boolean NOT NULL DEFAULT false,
    changed_at timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS webhook_app_health;
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS project_webhooks;
-- +goose StatementEnd
