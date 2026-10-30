-- +goose Up
-- The names a build's Dockerfile declares with ARG, in any stage, as the build
-- read them. A variable of the same name is passed to that build, so changing
-- one means the image is out of date and a deploy has to build again; any
-- other variable can be deployed on the image that is already there.
--
-- NULL is a build from before builds were given variables, or one that did
-- not say, and is treated as needing a build for any variable change. '*' is
-- a buildpacks build, which is given every variable.
ALTER TABLE builds ADD COLUMN build_args text[];

-- +goose Down
ALTER TABLE builds DROP COLUMN build_args;
