-- +goose Up
-- +goose StatementBegin

-- What a Git-sourced app was built from.
--
-- On the app rather than in a table of its own, for the reason the networking
-- toggles are: there is exactly one of each per app, and a row that always
-- exists is a join that always has to be remembered.
--
-- Empty on an image-sourced app, which is most of them. Not null with a default
-- rather than nullable: "no repository" and "a repository nobody set" are the
-- same state here, and two ways to spell it is two branches at every read.
ALTER TABLE apps ADD COLUMN repo_url text NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN repo_branch text NOT NULL DEFAULT '';

-- The directory inside the repository to build from, for a monorepo. Empty is
-- the repository root.
ALTER TABLE apps ADD COLUMN repo_subdir text NOT NULL DEFAULT '';

-- Builds.
--
-- One per deployment, and keyed to it: a build exists because somebody asked
-- for a deploy, and one that outlived its deployment would be a log nobody
-- could say what produced.
--
-- Owner-scoped like every other resource table. The owner is carried here
-- rather than reached through the deployment because the log is readable
-- output — the scoping has to be checkable in the query that reads it, not two
-- joins away.
CREATE TABLE builds (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    app_id uuid NOT NULL REFERENCES apps (id) ON DELETE CASCADE,

    -- The deployment this build is for. Cascade rather than restrict: deleting
    -- an app takes its deployments, and a build for a deployment that no
    -- longer exists is not a record worth keeping.
    deployment_id uuid NOT NULL REFERENCES deployments (id) ON DELETE CASCADE,

    -- What was built.
    repo_url text NOT NULL,
    repo_ref text NOT NULL,

    -- The commit actually built, once it is known. Empty until the clone.
    commit_sha text NOT NULL DEFAULT '',

    -- Where the result was pushed. Empty until the push succeeds, which is
    -- what makes it usable as the answer to "is there an image to deploy".
    image text NOT NULL DEFAULT '',

    -- running | succeeded | failed
    status text NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'succeeded', 'failed')),

    -- Why it failed, in the words somebody should read first. The log has the
    -- detail; this is the one line.
    message text NOT NULL DEFAULT '',

    -- The build log itself.
    --
    -- Kept, unlike container output, and this is the one place that is honest.
    -- A build is bounded and there is exactly one per deploy, so storing it
    -- costs a known amount — whereas a running container writes forever, which
    -- is why its log stays with it and dies with it.
    log text NOT NULL DEFAULT '',

    started_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);

CREATE INDEX builds_deployment_idx ON builds (deployment_id);
CREATE INDEX builds_app_started_idx ON builds (app_id, started_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS builds;
ALTER TABLE apps DROP COLUMN IF EXISTS repo_subdir;
ALTER TABLE apps DROP COLUMN IF EXISTS repo_branch;
ALTER TABLE apps DROP COLUMN IF EXISTS repo_url;
-- +goose StatementEnd
