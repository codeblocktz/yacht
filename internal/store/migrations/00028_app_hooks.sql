-- Deploy on push: one webhook per app, which a Git host calls when the branch
-- the app builds from moves.
--
-- A table of its own rather than columns on apps. It is optional, most apps
-- will never have one, and what it records — the last delivery and a push
-- waiting behind a deploy in flight — is about the hook rather than the app.
--
-- The secret is sealed with YACHT_SECRET_KEY like a secret variable. It has to
-- be recoverable, not hashed: an HMAC signature is checked by recomputing it.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE app_hooks (
    app_id           uuid PRIMARY KEY REFERENCES apps (id) ON DELETE CASCADE,
    owner_id         text NOT NULL,
    secret           bytea NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),

    -- A push that arrived while a deploy was in flight. It is admitted when
    -- that deploy ends, so a second push in quick succession is built rather
    -- than dropped. One slot, not a queue: the build reads the branch as it is
    -- then, which already includes every push before it.
    pending_at       timestamptz,
    pending_revision text NOT NULL DEFAULT '',

    last_delivery_at timestamptz,
    last_delivery    text NOT NULL DEFAULT ''
);

CREATE INDEX app_hooks_pending_idx ON app_hooks (pending_at)
    WHERE pending_at IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE app_hooks;

-- +goose StatementEnd
