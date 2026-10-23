-- Which pool of machines a team's apps are placed on.
--
-- A pool is a node label, yacht/pool, written when a machine joins. A team
-- given one has every app of its scheduled onto nodes carrying that label,
-- and tolerating the taint a dedicated pool's nodes carry. A hosting business
-- uses it to put a plan's customers on the machines sold for that plan.
--
-- A table of its own for the reason team_quotas is one: most installs never
-- place anyone, and what a row says is a policy about the team rather than a
-- fact of it. No row, and an empty pool, are both "anywhere" — the default.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE team_pools (
    owner_id   text PRIMARY KEY REFERENCES teams (id) ON DELETE CASCADE,

    -- A Kubernetes label value, the rule orchestrator.ValidatePool applies
    -- before anything is stored. Checked here only for length: the database
    -- is not where the rule lives, and two copies of a regular expression
    -- drift.
    pool       text NOT NULL DEFAULT '' CHECK (length(pool) <= 63),

    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS team_pools;
-- +goose StatementEnd
