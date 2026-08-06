-- name: CreateDeploymentOperation :one
INSERT INTO deployment_operations (
    owner_id, app_id, deployment_id, release_id, requires_build
)
VALUES (
    @owner_id, @app_id, @deployment_id, @release_id, @requires_build
)
RETURNING *;

-- Claim counting and selection must share this transaction-wide lock. Row
-- locks alone protect candidates, not the cluster-wide count: two connections
-- can otherwise lock different rows after both observed one free build slot.
-- name: LockDeploymentAdmission :exec
SELECT pg_advisory_xact_lock(1497542997);

-- name: ClaimNextDeploymentOperation :one
WITH candidate AS (
    SELECT o.id
    FROM deployment_operations o
    WHERE o.status = 'queued' AND o.checkpoint = 'claimed'
      AND (
          NOT o.requires_build OR o.checkpoint NOT IN ('claimed', 'building') OR
          (SELECT count(*) FROM deployment_operations live
           WHERE live.requires_build AND (
               live.status IN ('claimed', 'building')
               OR (live.status = 'queued' AND live.checkpoint = 'building')
           ))
              < sqlc.arg(max_concurrent_builds)::bigint
      )
    ORDER BY o.created_at, o.id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE deployment_operations o
SET status = o.checkpoint, claimed_at = now(),
    claim_token = gen_random_uuid(),
    lease_expires_at = now() + @lease_duration::interval,
    stage_started_at = CASE
        WHEN o.checkpoint = 'claimed' THEN now()
        ELSE o.stage_started_at
    END
FROM candidate
WHERE o.id = candidate.id
RETURNING o.*;

-- name: ClaimDeploymentOperation :one
UPDATE deployment_operations o
SET status = 'claimed', claimed_at = now(),
    claim_token = gen_random_uuid(),
    lease_expires_at = now() + @lease_duration::interval,
    stage_started_at = CASE
        WHEN o.checkpoint = 'claimed' THEN now()
        ELSE o.stage_started_at
    END
WHERE o.id = @id AND o.owner_id = @owner_id
  AND o.status = 'queued' AND o.checkpoint = 'claimed'
  AND (
      NOT o.requires_build OR o.checkpoint NOT IN ('claimed', 'building') OR
      (SELECT count(*) FROM deployment_operations live
       WHERE live.requires_build AND (
           live.status IN ('claimed', 'building')
           OR (live.status = 'queued' AND live.checkpoint = 'building')
       ))
          < sqlc.arg(max_concurrent_builds)::bigint
  )
RETURNING o.*;

-- name: FinishDeploymentOperation :execrows
UPDATE deployment_operations
SET status = @status, message = @message, finished_at = now(),
    claim_token = NULL, lease_expires_at = NULL
WHERE owner_id = @owner_id AND id = @id AND status = @expected_status
  AND claim_token = @claim_token;

-- name: RenewDeploymentOperationLease :execrows
UPDATE deployment_operations
SET lease_expires_at = now() + @lease_duration::interval
WHERE owner_id = @owner_id AND id = @id
  AND status IN ('claimed', 'building', 'applying', 'verifying')
  AND claim_token = @claim_token AND lease_expires_at > now();

-- name: TransitionDeploymentOperation :one
UPDATE deployment_operations
SET status = @next_status, checkpoint = @next_status, stage_started_at = now()
WHERE owner_id = @owner_id AND id = @id AND status = @expected_status
  AND claim_token = @claim_token AND lease_expires_at > now()
  AND (
      (@expected_status = 'claimed' AND @next_status IN ('building', 'applying'))
      OR (@expected_status = 'building' AND @next_status = 'applying')
      OR (@expected_status = 'applying' AND @next_status = 'verifying')
  )
RETURNING *;

-- name: SetClaimedOperationRelease :execrows
UPDATE deployment_operations o
SET release_id = @release_id
WHERE o.owner_id = @owner_id AND o.id = @id AND o.status = @expected_status
  AND o.claim_token = @claim_token
  AND EXISTS (
      SELECT 1 FROM app_releases r
      WHERE r.id = @release_id AND r.owner_id = @owner_id AND r.app_id = o.app_id
  );

-- name: ReclaimExpiredDeploymentOperations :many
UPDATE deployment_operations
SET status = 'queued', claimed_at = NULL,
    claim_token = NULL, lease_expires_at = NULL
WHERE status IN ('claimed', 'building', 'applying', 'verifying')
  AND lease_expires_at <= now()
RETURNING *;

-- name: CancelDeploymentOperation :one
UPDATE deployment_operations
SET status = 'cancelled', cancelled_at = now(), finished_at = now(),
    message = 'cancelled by request', claim_token = NULL, lease_expires_at = NULL
WHERE owner_id = @owner_id AND id = @id
  AND status IN ('queued', 'claimed', 'building', 'applying', 'verifying')
RETURNING *;

-- name: GetBuildRecoveryOperation :one
SELECT * FROM deployment_operations
WHERE owner_id = @owner_id AND deployment_id = @deployment_id
FOR UPDATE;

-- name: RecoverBuiltDeploymentOperation :one
UPDATE deployment_operations o
SET release_id = @release_id,
    status = 'applying', checkpoint = 'applying',
    claimed_at = now(), stage_started_at = now(),
    claim_token = gen_random_uuid(),
    lease_expires_at = now() + @lease_duration::interval
WHERE o.owner_id = @owner_id AND o.deployment_id = @deployment_id
  AND o.status = 'queued' AND o.checkpoint IN ('claimed', 'building')
  AND EXISTS (
      SELECT 1 FROM app_releases r
      WHERE r.id = @release_id AND r.owner_id = @owner_id AND r.app_id = o.app_id
  )
RETURNING o.*;

-- name: FailQueuedBuildOperation :execrows
UPDATE deployment_operations
SET status = 'failed', message = @message, finished_at = now()
WHERE owner_id = @owner_id AND deployment_id = @deployment_id
  AND status = 'queued' AND checkpoint = 'building';

-- name: GetDeploymentOperation :one
SELECT * FROM deployment_operations
WHERE owner_id = @owner_id AND id = @id;

-- name: GetDeploymentOperationByDeployment :one
SELECT * FROM deployment_operations
WHERE owner_id = @owner_id AND deployment_id = @deployment_id;

-- The live operation is the only candidate allowed to displace an app's
-- active release. Steady-state convergence must stand down while one exists.
-- name: GetLiveDeploymentOperationForApp :one
SELECT * FROM deployment_operations
WHERE owner_id = @owner_id AND app_id = @app_id
  AND status IN ('queued', 'claimed', 'building', 'applying', 'verifying')
ORDER BY created_at, id
LIMIT 1;

-- Applying and verifying are recovered from workload observation, never by
-- the ordinary admission worker. The checkpoint tells this claimant which
-- stage the dead worker had durably entered.
-- name: ClaimRecoverableDeploymentOperation :one
UPDATE deployment_operations
SET status = checkpoint, claimed_at = now(),
    claim_token = gen_random_uuid(),
    lease_expires_at = now() + @lease_duration::interval
WHERE owner_id = @owner_id AND id = @id
  AND status = 'queued' AND checkpoint IN ('applying', 'verifying')
RETURNING *;

-- A cluster outage is not a deployment failure. Give observation recovery
-- back to the queue without changing its checkpoint or verification budget.
-- name: ReleaseRecoverableDeploymentOperation :execrows
UPDATE deployment_operations
SET status = 'queued', claimed_at = NULL,
    claim_token = NULL, lease_expires_at = NULL
WHERE owner_id = @owner_id AND id = @id
  AND status = @expected_status AND claim_token = @claim_token;

-- name: ListDeploymentOperations :many
SELECT * FROM deployment_operations
WHERE owner_id = @owner_id
ORDER BY created_at DESC, id DESC
LIMIT @result_limit;
