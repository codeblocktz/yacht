-- +goose Up
-- +goose StatementBegin

-- The numeric uid a built image runs as.
--
-- Kubernetes will not start a container with runAsNonRoot unless it can prove
-- the user is not root, and it cannot prove that from a name: an image whose
-- Dockerfile ends "USER node" fails with CreateContainerConfigError, having
-- built and pushed and pulled perfectly.
--
-- So the build resolves it and it is recorded here. Zero means unknown, which
-- is every app that was not built from a repository.
ALTER TABLE apps ADD COLUMN run_as_user bigint NOT NULL DEFAULT 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE apps DROP COLUMN IF EXISTS run_as_user;
-- +goose StatementEnd
