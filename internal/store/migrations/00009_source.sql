-- +goose Up
-- +goose StatementBegin

-- What the app was created from.
--
-- Stored rather than inferred, because it decides things nothing can work out
-- from the image afterwards: whether a hostname is issued, which uid the
-- container runs as, and where its data lives. An app deployed before this
-- existed came from an image, which is what the default says.
ALTER TABLE apps ADD COLUMN source text NOT NULL DEFAULT 'image';

-- Whether the workload is reachable from outside the cluster.
--
-- A database speaks its own protocol on its own port, so an HTTP hostname
-- pointed at it would be a route to something that cannot answer. Kept as its
-- own column rather than derived from the source, so a future source does not
-- have to be added to a list somewhere else to be routed correctly.
ALTER TABLE apps ADD COLUMN internal boolean NOT NULL DEFAULT false;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE apps DROP COLUMN IF EXISTS internal;
ALTER TABLE apps DROP COLUMN IF EXISTS source;
-- +goose StatementEnd
