-- +goose Up
-- The most a team may keep in its buckets, every bucket together, in bytes.
-- Zero is unlimited, as in every other column. Not committed up front like a
-- volume: what a bucket holds grows with every upload, so this is enforced by
-- the store itself, as a size limit on each bucket (app.limitBuckets).
ALTER TABLE team_quotas ADD COLUMN bucket_bytes bigint NOT NULL DEFAULT 0 CHECK (bucket_bytes >= 0);

-- +goose Down
ALTER TABLE team_quotas DROP COLUMN bucket_bytes;
