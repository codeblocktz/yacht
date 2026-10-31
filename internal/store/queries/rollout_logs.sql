-- A failed rollout's last output, owner-scoped like the deployment.

-- name: KeepRolloutLog :exec
-- Replaced rather than appended: a rollout verified again after its worker
-- went away fails with the same pod, read again later, which is the fuller
-- copy.
INSERT INTO deployment_rollout_logs (deployment_id, owner_id, pod, previous, output)
VALUES (@deployment_id, @owner_id, @pod, @previous, @output)
ON CONFLICT (deployment_id) DO UPDATE
SET pod = EXCLUDED.pod, previous = EXCLUDED.previous, output = EXCLUDED.output, created_at = now()
WHERE deployment_rollout_logs.owner_id = EXCLUDED.owner_id;

-- name: GetRolloutLog :one
SELECT pod, previous, output FROM deployment_rollout_logs
WHERE owner_id = @owner_id AND deployment_id = @deployment_id;
