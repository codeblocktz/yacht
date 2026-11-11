-- +goose Up
-- Every custom domain is checked again now rather than at its next scheduled
-- check, which for one that has been live a while is hours away.
--
-- This release refuses the install's own addresses as custom domains — the
-- dashboard's, the registry's, the CNAME target and the rest — at the claim
-- and at every check. A claim on one made before it is still routed until its
-- next check withdraws it, and that should be the first thing the checker
-- does rather than something it gets to eventually. The checker works through
-- them twenty at a time, so this is a burst of lookups, not a stampede.
UPDATE domains SET next_check_at = now() WHERE NOT managed;

-- +goose Down
-- Nothing to undo: the schedule moves on from whatever the checks find.
SELECT 1;
