-- +goose Up
-- +goose StatementBegin

-- Kept separate from invitations rather than folded into them. The two mean
-- different things — one proves an address, the other grants a role — expire
-- on different timescales, and are consumed by different flows. One table
-- serving both would need a discriminator column and a partial index per
-- meaning, which is the same separation with more ways to get it wrong.
CREATE TABLE magic_links (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash  bytea       NOT NULL,
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX magic_links_token_hash_key ON magic_links (token_hash);
CREATE INDEX magic_links_user_id_idx ON magic_links (user_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS magic_links;
-- +goose StatementEnd
