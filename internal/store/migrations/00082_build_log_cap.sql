-- +goose Up
-- Whether a build's log reached the install's limit and stopped growing. Its
-- own column rather than a look at the log's tail, so the append that crosses
-- the limit and every one after it decide in the same row lock: the marker is
-- written once, and nothing is appended after it.
ALTER TABLE builds ADD COLUMN log_truncated boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE builds DROP COLUMN log_truncated;
