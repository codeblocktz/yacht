-- A deployment's pre-deploy transcript, owner-scoped like the deployment.

-- name: AppendPreDeployLog :exec
-- Appended rather than replaced: a run retried after its worker went away
-- adds to what the first attempt printed, which is history too.
INSERT INTO deployment_pre_deploy_logs (deployment_id, owner_id, output)
VALUES (@deployment_id, @owner_id, @output)
ON CONFLICT (deployment_id) DO UPDATE
SET output = deployment_pre_deploy_logs.output || EXCLUDED.output, updated_at = now()
WHERE deployment_pre_deploy_logs.owner_id = EXCLUDED.owner_id;

-- name: GetPreDeployLog :one
SELECT output FROM deployment_pre_deploy_logs
WHERE owner_id = @owner_id AND deployment_id = @deployment_id;
