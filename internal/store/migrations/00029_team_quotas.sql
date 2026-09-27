-- How much each team on an install may commit: apps, CPU, memory and storage.
--
-- Set by the install's operator, not by the team. A team hosted on somebody
-- else's cluster sharing it with other teams is exactly the case where one of
-- them deploying forty replicas is everyone's problem, and the person who can
-- see every team is the one who decides how the cluster is shared.
--
-- A table of its own rather than columns on teams. Most installs host one team
-- and will never have a row here, and what a row says is a policy about the
-- team rather than a fact of it.
--
-- owner_id is the primary key and the whole scoping: one row per team, gone
-- with the team.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE team_quotas (
    owner_id text PRIMARY KEY REFERENCES teams (id) ON DELETE CASCADE,

    -- Zero is unlimited, in every column, and so is a team with no row at
    -- all. Nullable columns would say the same thing a second way, and a
    -- quota of zero apps is not a limit anybody means — a team that should
    -- deploy nothing is one to remove, not one to starve.
    max_apps      integer NOT NULL DEFAULT 0 CHECK (max_apps >= 0),

    -- The sum of each app's replicas times its limit, in the units the
    -- cluster is given. Limits rather than requests: a limit is what a
    -- workload may actually take from the node it lands on, and what the
    -- other teams on that node are giving up.
    cpu_millis    bigint  NOT NULL DEFAULT 0 CHECK (cpu_millis >= 0),
    memory_bytes  bigint  NOT NULL DEFAULT 0 CHECK (memory_bytes >= 0),

    -- The sum of every volume's size. A claim's size is reserved whether or
    -- not anything is written to it.
    storage_bytes bigint  NOT NULL DEFAULT 0 CHECK (storage_bytes >= 0),

    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS team_quotas;
-- +goose StatementEnd
