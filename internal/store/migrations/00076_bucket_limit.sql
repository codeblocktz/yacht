-- +goose Up
-- What the engine needs to share the bucket store's disk between every
-- bucket on the install (app.limitBuckets).
--
-- limit_bytes is the most a bucket may hold as last set on the store, zero
-- for none: never less than the store enforces, so a disk that cannot be read
-- only ever tightens it, and a bucket measured past it was written past it.
--
-- uploading_bytes is what its unfinished multipart uploads hold, which
-- size_bytes now includes: the store keeps those parts on its disk, and checks
-- a bucket's limit only when an upload is completed.
--
-- wants_room_at is when it last grew, or was about to be written through the
-- Files tab: a bucket being written to may draw on the store's free room
-- beyond the small part every bucket keeps.
ALTER TABLE buckets
    ADD COLUMN limit_bytes bigint NOT NULL DEFAULT 0 CHECK (limit_bytes >= 0),
    ADD COLUMN uploading_bytes bigint NOT NULL DEFAULT 0 CHECK (uploading_bytes >= 0),
    ADD COLUMN wants_room_at timestamptz;

-- +goose Down
ALTER TABLE buckets
    DROP COLUMN wants_room_at,
    DROP COLUMN uploading_bytes,
    DROP COLUMN limit_bytes;
