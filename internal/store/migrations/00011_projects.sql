-- +goose Up
-- +goose StatementBegin

-- A project is the canvas an app is drawn on.
--
-- It groups apps that belong to one system, which is the unit people actually
-- reason about: a web service, its worker and its database are one thing to
-- deploy and one picture to look at, even though they are three workloads.
--
-- Deliberately not a namespace. The Kubernetes namespace is still the team's,
-- so app names stay unique per team and every existing address — the in-cluster
-- host another app connects to, the URL somebody bookmarked — keeps meaning
-- what it meant. A project that renamed workloads would break both.
CREATE TABLE projects (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id   text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,

    -- Addressable, so a project has a URL somebody can share.
    slug       text NOT NULL CHECK (slug ~ '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$' AND length(slug) <= 63),
    name       text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    UNIQUE (owner_id, slug)
);

-- Nullable, and every read treats null as "the team's default project".
--
-- The alternative — backfill a project for every team now and make the column
-- NOT NULL — needs this migration to invent a project for teams that have
-- never opened the app, including every team created after it runs. Resolving
-- null lazily means the default is created by the code that needs one, on a
-- path that already handles failure.
ALTER TABLE apps ADD COLUMN project_id uuid REFERENCES projects (id) ON DELETE SET NULL;

CREATE INDEX apps_project_id_idx ON apps (project_id);

-- Where the card sits on the canvas, once somebody has moved it.
--
-- Null means nobody has: the server lays that app out from its dependencies
-- instead. Storing an automatic position eagerly would freeze the first
-- arrangement, so adding a database later would leave it overlapping whatever
-- was already there rather than making room.
ALTER TABLE apps ADD COLUMN canvas_x integer;
ALTER TABLE apps ADD COLUMN canvas_y integer;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE apps DROP COLUMN IF EXISTS canvas_y;
ALTER TABLE apps DROP COLUMN IF EXISTS canvas_x;
ALTER TABLE apps DROP COLUMN IF EXISTS project_id;
DROP TABLE IF EXISTS projects;
-- +goose StatementEnd
