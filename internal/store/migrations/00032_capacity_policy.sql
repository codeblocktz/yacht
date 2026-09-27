-- How much of the whole install may be sold, and every change refused for it.
--
-- Quotas bound each team. Nothing bounded their sum except the machines, and a
-- business that buys machines and sells room on them must not sell room it
-- has not bought. This is the operator's answer to how much that is: the
-- schedulable nodes' allocatable, oversold by a ratio, less a reserve kept for
-- failover and rollouts.

-- +goose Up
-- +goose StatementBegin

-- One row, by constraint rather than by convention, like cluster_join: a
-- second row nobody noticed is how an install ends up enforcing a policy
-- nobody can see on the page that edits it. Inserted here, so that every
-- install has it and the admission check can lock it without first asking
-- whether it exists.
CREATE TABLE capacity_policy (
    id integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),

    -- Off on every install that existed before this, which then behaves
    -- exactly as it did. Turning it on is the operator's decision.
    enforce boolean NOT NULL DEFAULT false,

    -- How far limits may be oversold. Limits are ceilings, not reservations,
    -- and few workloads sit at theirs — so CPU is routinely sold more than
    -- once. Memory is not throttled when it runs out; a pod over it is killed,
    -- which is why the page says so beside the field. Below 1 is refused: the
    -- reserve is how room is held back.
    cpu_commit_ratio    double precision NOT NULL DEFAULT 1
        CHECK (cpu_commit_ratio >= 1 AND cpu_commit_ratio <= 10),
    memory_commit_ratio double precision NOT NULL DEFAULT 1
        CHECK (memory_commit_ratio >= 1 AND memory_commit_ratio <= 10),

    -- Kept free for a node failing, a drain, and a rollout's surge. Below 100
    -- so there is always something left to sell.
    reserve_percent integer NOT NULL DEFAULT 0
        CHECK (reserve_percent >= 0 AND reserve_percent <= 90),

    -- When the operator is warned. 80 was the capacity page's fixed line
    -- before this was a setting.
    warn_percent integer NOT NULL DEFAULT 80
        CHECK (warn_percent >= 1 AND warn_percent <= 100),

    -- Volume capacity, typed by the operator because nothing can read it: a
    -- node's disk is not what a storage class provisions from. Zero is not
    -- counted at all.
    storage_bytes bigint NOT NULL DEFAULT 0 CHECK (storage_bytes >= 0),

    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO capacity_policy (id) VALUES (1);

-- Demand turned away. The customer is told there was no room; this is how the
-- operator learns that somebody wanted some, which is the signal to buy more.
--
-- owner_id is the team that was refused, and goes with it. Shortfall is how
-- much more than was sellable the change would have taken, in the resource's
-- own unit: millicores for cpu, bytes for memory and storage.
CREATE TABLE capacity_refusals (
    id        uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id  text        NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    resource  text        NOT NULL CHECK (resource IN ('cpu', 'memory', 'storage')),
    shortfall bigint      NOT NULL CHECK (shortfall > 0),
    at        timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX capacity_refusals_at_idx ON capacity_refusals (at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS capacity_refusals;
DROP TABLE IF EXISTS capacity_policy;
-- +goose StatementEnd
