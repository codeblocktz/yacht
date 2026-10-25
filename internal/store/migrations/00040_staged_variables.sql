-- Variables that wait for a deploy, and variables a whole project shares.
--
-- Numbered 40 because 38 and 39 belong to work merged before this; goose
-- refuses a lower number arriving after a higher one has run.

-- +goose Up
-- +goose StatementBegin

-- An edit to an app's variables that has not been deployed yet.
--
-- Its own table rather than a pending column beside each variable, because the
-- variables table is what a deploy reads: everything that resolves a
-- reference, opens a secret or snapshots a release reads it, and every one of
-- them must see what is running rather than what somebody is halfway through
-- typing. A secret saved here is not in the cluster until it is deployed, and
-- discarding it leaves the one that is running exactly as it was — which an
-- edit made in place could promise for neither.
CREATE TABLE staged_variables (
    owner_id text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    app_id   uuid NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    key      text NOT NULL CHECK (key ~ '^[A-Za-z_][A-Za-z0-9_]*$' AND length(key) <= 128),

    -- Removed stages taking the variable away; the value columns are then
    -- empty, because there is nothing to deploy for it but its absence.
    removed boolean NOT NULL DEFAULT false,
    value   text    NOT NULL DEFAULT '',
    sealed  bytea,
    secret  boolean NOT NULL DEFAULT false,

    updated_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (app_id, key),

    -- The variables table's invariant, plus a removal carrying nothing.
    CONSTRAINT staged_variables_storage_matches_kind CHECK (
        (removed AND NOT secret AND sealed IS NULL AND value = '') OR
        (NOT removed AND secret AND sealed IS NOT NULL AND value = '') OR
        (NOT removed AND NOT secret AND sealed IS NULL)
    )
);

CREATE INDEX staged_variables_owner_id_idx ON staged_variables (owner_id);

-- A project's shared variables, which its apps use as ${{shared.KEY}}.
--
-- Stored like an app's: a row each, sealed when secret, under the same rules
-- for what a name may be. An app never copies one — the reference is resolved
-- when the app deploys — so a value typed once here is the only copy of it.
CREATE TABLE project_variables (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id   text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    key        text NOT NULL CHECK (key ~ '^[A-Za-z_][A-Za-z0-9_]*$' AND length(key) <= 128),

    value  text  NOT NULL DEFAULT '',
    sealed bytea,
    secret boolean NOT NULL DEFAULT false,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT project_variables_storage_matches_kind CHECK (
        (secret AND sealed IS NOT NULL AND value = '') OR
        (NOT secret AND sealed IS NULL)
    )
);

CREATE UNIQUE INDEX project_variables_project_key_key ON project_variables (project_id, key);
CREATE INDEX project_variables_owner_id_idx ON project_variables (owner_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS project_variables;
DROP TABLE IF EXISTS staged_variables;
-- +goose StatementEnd
