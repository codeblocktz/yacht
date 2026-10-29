-- +goose Up
-- The most a bucket may hold, as last set on the store (app.limitBuckets), in
-- bytes; zero for none set. Kept so that the room one team's buckets are given
-- can leave out what other teams' buckets were already given and have not yet
-- written: the store's free room, given twice, could be written twice.
ALTER TABLE buckets ADD COLUMN limit_bytes bigint NOT NULL DEFAULT 0 CHECK (limit_bytes >= 0);

-- +goose Down
ALTER TABLE buckets DROP COLUMN limit_bytes;
