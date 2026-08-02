-- +goose Up
-- +goose StatementBegin

-- Whether this registry is served over plain HTTP.
--
-- Not a hack around TLS: an internal registry on a private network is a normal
-- deployment, and every tool that consumes one has this switch — Docker calls
-- it insecure-registries, containerd configures the endpoint scheme, BuildKit
-- takes registry.insecure on the output.
--
-- Default false. A registry assumed to be plain HTTP when it is not would
-- downgrade a connection that carries a push credential, so the safe answer is
-- the one nobody has to choose.
ALTER TABLE platform_registry ADD COLUMN insecure boolean NOT NULL DEFAULT false;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE platform_registry DROP COLUMN IF EXISTS insecure;
-- +goose StatementEnd
