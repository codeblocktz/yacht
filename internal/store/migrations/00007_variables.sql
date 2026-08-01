-- +goose Up
-- +goose StatementBegin

-- Environment variables, moved out of the app record.
--
-- They lived in apps.env as a jsonb map, which made every value plaintext in
-- Postgres and therefore in every backup. A row per variable is what allows one
-- of them to be sealed while its neighbour stays readable — a map has one
-- storage rule for all of its entries.
CREATE TABLE variables (
    id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    app_id   uuid NOT NULL REFERENCES apps (id) ON DELETE CASCADE,

    -- What a process sees. Constrained to what a shell will actually export:
    -- a name with a dash in it can be set in a pod spec and then not read by
    -- most programs, which is a bug nobody finds quickly.
    key text NOT NULL CHECK (key ~ '^[A-Za-z_][A-Za-z0-9_]*$' AND length(key) <= 128),

    -- Exactly one of these holds the value.
    --
    -- A secret's plaintext is never written here: sealed carries AES-GCM
    -- ciphertext, and the key that opens it lives in configuration rather than
    -- in the database it protects. Splitting the columns rather than sharing
    -- one means a query cannot accidentally return a secret as though it were
    -- readable.
    value  text  NOT NULL DEFAULT '',
    sealed bytea,

    secret     boolean     NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- The invariant the two columns exist to express, enforced here so no
    -- writer can leave a secret readable or a plain variable unreadable.
    CONSTRAINT variables_storage_matches_kind CHECK (
        (secret AND sealed IS NOT NULL AND value = '') OR
        (NOT secret AND sealed IS NULL)
    )
);

CREATE UNIQUE INDEX variables_app_key_key ON variables (app_id, key);
CREATE INDEX variables_owner_id_idx ON variables (owner_id);

-- Carry the existing variables across. They were plaintext, so they arrive as
-- plaintext: calling them secret now would claim a protection they never had.
INSERT INTO variables (owner_id, app_id, key, value, secret)
SELECT a.owner_id, a.id, e.key, e.value, false
FROM apps a, jsonb_each_text(a.env) AS e(key, value)
WHERE e.key ~ '^[A-Za-z_][A-Za-z0-9_]*$';

ALTER TABLE apps DROP COLUMN env;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE apps ADD COLUMN env jsonb NOT NULL DEFAULT '{}'::jsonb;

-- Only the readable ones can go back. A sealed value has nowhere to live in a
-- jsonb map, and writing its ciphertext there would be worse than losing it.
UPDATE apps a SET env = COALESCE((
    SELECT jsonb_object_agg(v.key, v.value)
    FROM variables v WHERE v.app_id = a.id AND NOT v.secret
), '{}'::jsonb);

DROP TABLE IF EXISTS variables;
-- +goose StatementEnd
