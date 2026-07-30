-- +goose Up
-- +goose StatementBegin

-- Every table carries owner_id, and every unique constraint is scoped by it.
--
-- The engine runs with exactly one owner, so this column holds the same value
-- in every row and looks redundant. It is not. An application wrapping the
-- engine sets it per tenant, and at that point ownership is already a cheap
-- indexed predicate on the table being queried rather than a join back through
-- two others. A predicate is a check that gets written; a join is a check that
-- gets skipped, and the skipped one is how tenants read each other's data.
--
-- Adding this later would mean a migration across live data plus an audit of
-- every existing query. Adding it now costs a column.

CREATE TABLE owners (
    id           text PRIMARY KEY,
    display_name text        NOT NULL DEFAULT '',
    email        text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE apps (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id   text        NOT NULL REFERENCES owners (id) ON DELETE CASCADE,

    -- Kubernetes object name. Constrained here as well as in the orchestrator
    -- so a bad name cannot reach a cluster even if it bypasses that layer.
    name       text        NOT NULL CHECK (name ~ '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$' AND length(name) <= 63),
    namespace  text        NOT NULL CHECK (namespace ~ '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$' AND length(namespace) <= 63),

    image      text        NOT NULL,
    replicas   integer     NOT NULL DEFAULT 1 CHECK (replicas >= 0),
    port       integer     NOT NULL DEFAULT 0 CHECK (port >= 0 AND port <= 65535),
    env        jsonb       NOT NULL DEFAULT '{}'::jsonb,

    cpu_request    text NOT NULL DEFAULT '',
    cpu_limit      text NOT NULL DEFAULT '',
    memory_request text NOT NULL DEFAULT '',
    memory_limit   text NOT NULL DEFAULT '',

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Scoped by owner, not global. Two owners may both have an app called "api";
-- a global unique index here is exactly the bug that makes a single-tenant
-- schema impossible to extend.
CREATE UNIQUE INDEX apps_owner_name_key ON apps (owner_id, name);
CREATE INDEX apps_owner_id_idx ON apps (owner_id);

-- A namespace must belong to exactly one owner. Without this, two owners can
-- be handed the same namespace and each deletes the other's workloads.
CREATE UNIQUE INDEX apps_namespace_key ON apps (namespace);

CREATE TABLE deployments (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id    text        NOT NULL REFERENCES owners (id) ON DELETE CASCADE,
    app_id      uuid        NOT NULL REFERENCES apps (id) ON DELETE CASCADE,

    image       text        NOT NULL,
    revision    text        NOT NULL DEFAULT '',
    status      text        NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'cancelled')),
    message     text        NOT NULL DEFAULT '',

    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);

CREATE INDEX deployments_owner_id_idx ON deployments (owner_id);
CREATE INDEX deployments_app_id_started_at_idx ON deployments (app_id, started_at DESC);

CREATE TABLE domains (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id   text        NOT NULL REFERENCES owners (id) ON DELETE CASCADE,
    app_id     uuid        NOT NULL REFERENCES apps (id) ON DELETE CASCADE,

    host       text        NOT NULL,
    tls        boolean     NOT NULL DEFAULT true,
    verified   boolean     NOT NULL DEFAULT false,

    created_at timestamptz NOT NULL DEFAULT now()
);

-- Hostnames are globally unique by nature: DNS has one owner per name, and two
-- rows claiming the same host means whichever ingress reconciles last wins.
-- This is the one place a global constraint is correct.
CREATE UNIQUE INDEX domains_host_key ON domains (lower(host));
CREATE INDEX domains_owner_id_idx ON domains (owner_id);
CREATE INDEX domains_app_id_idx ON domains (app_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS domains;
DROP TABLE IF EXISTS deployments;
DROP TABLE IF EXISTS apps;
DROP TABLE IF EXISTS owners;
-- +goose StatementEnd
