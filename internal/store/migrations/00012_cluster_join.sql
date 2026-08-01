-- +goose Up
-- +goose StatementBegin

-- How a machine joins this cluster.
--
-- Two deliberate departures from every other table here.
--
-- There is no owner_id. A cluster is not a team's: a node somebody adds runs
-- every team's workloads and can read the secrets mounted into them. Giving
-- this row an owner would encode the claim that each team has its own cluster,
-- which is false, and would let one team's admin quietly widen the cluster for
-- everyone else.
--
-- And there is exactly one row, by constraint rather than by convention. A
-- second row nobody noticed is how a join page starts handing out a token that
-- was rotated last month.
CREATE TABLE cluster_join (
    id integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),

    -- The address an agent connects back to, e.g. https://10.0.0.1:6443.
    server_url text NOT NULL,

    -- Sealed with the same AES-GCM keeper that protects app secrets, so a
    -- database dump is not a way to add a machine to the cluster.
    token_sealed bytea NOT NULL,

    updated_at timestamptz NOT NULL DEFAULT now(),

    -- Who last changed it. This is the credential that grants cluster
    -- membership; when it changes, who changed it is the first thing anyone
    -- will want to know.
    updated_by uuid REFERENCES users (id) ON DELETE SET NULL
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS cluster_join;
-- +goose StatementEnd
