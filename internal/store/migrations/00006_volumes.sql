-- +goose Up
-- +goose StatementBegin

-- Storage a workload keeps across redeploys.
--
-- A volume belongs to exactly one app. Sharing one between apps is a different
-- feature with a different ownership model — a claim two apps can mount is a
-- claim neither can be said to own — and is deliberately not what this is.
CREATE TABLE volumes (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id   text        NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    app_id     uuid        NOT NULL REFERENCES apps (id) ON DELETE CASCADE,

    -- Becomes part of the PersistentVolumeClaim name, so it is constrained the
    -- same way an app name is: what Kubernetes accepts as an object name.
    name       text        NOT NULL CHECK (name ~ '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'
                                           AND length(name) <= 40),

    mount_path text        NOT NULL CHECK (mount_path LIKE '/%' AND mount_path <> '/'),

    -- Bytes rather than a Kubernetes quantity string, so comparing a new size
    -- to the old one is arithmetic instead of parsing. Expansion is the only
    -- direction available, and that comparison is what enforces it.
    size_bytes bigint      NOT NULL CHECK (size_bytes > 0),

    -- Empty means the cluster's default StorageClass.
    class      text        NOT NULL DEFAULT '',

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One name per app: the name is how a person refers to it.
CREATE UNIQUE INDEX volumes_app_name_key ON volumes (app_id, name);

-- One claim per mount path. Two volumes at the same path is a workload where
-- one silently wins, and which one is not something the operator chose.
CREATE UNIQUE INDEX volumes_app_mount_key ON volumes (app_id, mount_path);

CREATE INDEX volumes_owner_id_idx ON volumes (owner_id);
CREATE INDEX volumes_app_id_idx ON volumes (app_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS volumes;
-- +goose StatementEnd
