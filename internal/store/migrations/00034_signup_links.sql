-- Open sign-up: a link that creates an account when it is followed.
--
-- A sign-in link names a user, so it can only be issued to somebody who
-- already exists. An install that lets strangers sign up still must not let
-- the sign-in form fill the user table, so a stranger is given one of these
-- instead: it names an address, not a user, and the account is created only
-- when the link is followed — by whoever can read that mailbox.
--
-- Kept apart from magic_links rather than making its user_id nullable, for the
-- reason magic_links is kept apart from invitations: the two are consumed by
-- different flows, and a row that is sometimes one and sometimes the other is
-- the same separation with more ways to get it wrong.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE signup_links (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    email       text        NOT NULL,
    token_hash  bytea       NOT NULL,
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX signup_links_token_hash_key ON signup_links (token_hash);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS signup_links;
-- +goose StatementEnd
