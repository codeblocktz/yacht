-- +goose Up
-- +goose StatementBegin

-- Per-app routing choices.
--
-- On the app rather than in the install-wide DNS table because these really are
-- per-app: one service can be HTTPS-only while another is still being moved
-- over, and the annotations they produce are written per Ingress.
--
-- Both default to on. HTTPS-only is what anybody would want and the opposite is
-- the deliberate choice; CNAME-only keeps the cluster's node addresses out of
-- public DNS, which is a disclosure nobody should have to opt out of by
-- default.
ALTER TABLE apps ADD COLUMN https_only boolean NOT NULL DEFAULT true;
ALTER TABLE apps ADD COLUMN cname_only boolean NOT NULL DEFAULT true;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE apps DROP COLUMN IF EXISTS cname_only;
ALTER TABLE apps DROP COLUMN IF EXISTS https_only;
-- +goose StatementEnd
