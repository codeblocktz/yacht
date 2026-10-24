-- Regions: where an app lives.
--
-- A region is a node label, topology.kubernetes.io/region, written when a
-- machine joins. Which regions exist is the nodes' to say. What a node cannot
-- say about its region is kept here: what to call it, where its traffic
-- enters (its edge), which StorageClass its volumes are provisioned from, and
-- whether new apps may be placed in it. A region with nodes and no row is
-- unconfigured; a row with no nodes is a region nobody can be placed in yet.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE regions (
    -- The label value. orchestrator.ValidateRegion is the rule, applied
    -- before anything is stored; checked here only for length, like a pool,
    -- because two copies of a regular expression drift.
    name           text PRIMARY KEY CHECK (length(name) BETWEEN 1 AND 63),

    -- What people are shown. Empty shows the name.
    display_name   text NOT NULL DEFAULT '' CHECK (length(display_name) <= 80),

    -- The edge: where the region's ingress answers. A hostname, which is what
    -- a custom domain in this region is told to CNAME to, and the addresses
    -- behind it, for an application that writes A records. Either may be
    -- empty; both empty is a region whose traffic enters wherever the
    -- install's does.
    edge_host      text   NOT NULL DEFAULT '' CHECK (length(edge_host) <= 253),
    edge_addresses text[] NOT NULL DEFAULT '{}',

    -- The StorageClass a volume created in this region is provisioned from.
    -- Empty is the cluster's default. Recorded on each volume as it is made,
    -- so changing it reaches new volumes and never an existing claim, whose
    -- class Kubernetes will not change.
    storage_class  text NOT NULL DEFAULT '' CHECK (length(storage_class) <= 253),

    -- Whether new apps may be placed here. Closing a region moves nothing:
    -- what is already there stays, and nothing new arrives.
    open           boolean NOT NULL DEFAULT true,

    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- The region an app lives in. Null on every app made before regions existed,
-- and on every app of an install whose nodes carry none — which is how such an
-- install goes on behaving exactly as it did. Not a foreign key: the region
-- belongs to the nodes, and an app recorded in one must not be blocked by a
-- row an operator has not written yet.
ALTER TABLE apps ADD COLUMN region text
    CHECK (region IS NULL OR length(region) BETWEEN 1 AND 63);

CREATE INDEX apps_region_idx ON apps (region);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS apps_region_idx;
ALTER TABLE apps DROP COLUMN IF EXISTS region;
DROP TABLE IF EXISTS regions;
-- +goose StatementEnd
