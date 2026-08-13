-- Platform-issued hostnames. Custom domains get their own queries when that
-- sub-project lands; keeping them in one file rather than in apps.sql is so
-- that apps.sql stays about apps.

-- Issues or reissues the platform hostname for an app.
--
-- The conflict target is the partial index, so a second call for the same app
-- moves its hostname rather than adding one. A collision on the global host
-- index is a different conflict and is deliberately left to raise, because two
-- apps claiming one hostname is a real error, not something to paper over.
--
-- The state is 'routed' because a name the platform issued needs no proof of
-- ownership; requiring proof would mean inventing a verification step for a
-- name we already control. verified follows from it and is not written here —
-- it is generated from state, so that the routing gate and the state on screen
-- cannot disagree.
-- name: UpsertManagedDomain :one
INSERT INTO domains (owner_id, app_id, host, tls, state, managed)
VALUES (@owner_id, @app_id, lower(@host), @tls, 'routed', true)
ON CONFLICT (app_id) WHERE managed
DO UPDATE SET host = lower(@host), tls = @tls
RETURNING *;

-- name: ListDomainsByApp :many
SELECT * FROM domains
WHERE app_id = @app_id
ORDER BY managed DESC, host;

-- Releases an app's platform hostname.
--
-- Deleting rather than leaving the row is what makes the feature reversible:
-- hostnames are globally unique, so a retired row keeps the name reserved
-- against every other app forever.
-- name: DeleteManagedDomain :exec
DELETE FROM domains
WHERE app_id = @app_id AND managed;

-- name: GetManagedDomain :one
SELECT * FROM domains
WHERE app_id = @app_id AND managed
LIMIT 1;

-- ---------------------------------------------------------- custom domains

-- Claims a hostname for an app, unverified.
--
-- Deliberately not an upsert on host: the global unique index is what makes two
-- teams claiming one name an error rather than a silent transfer, and papering
-- over it here is exactly how a domain gets stolen.
--
-- next_check_at defaults to now(), so a claim is picked up by the checker on its
-- next pass rather than waiting for somebody to press a button. That is the
-- whole difference between this and what it replaced.
-- name: CreateCustomDomain :one
INSERT INTO domains (owner_id, app_id, host, tls, state, managed, verify_target)
VALUES (@owner_id, @app_id, lower(@host), true, 'pending', false, @verify_target)
RETURNING *;

-- name: ListCustomDomains :many
SELECT * FROM domains
WHERE owner_id = @owner_id AND app_id = @app_id AND NOT managed
ORDER BY host;

-- Records what a check saw.
--
-- Not scoped by owner, and deliberately: the background checker works through
-- every install's rows by id, having already claimed them below. Owner scoping
-- protects a request that names a row; this is not one. Every path that reaches
-- here from a request goes through GetCustomDomain first, which is scoped.
--
-- The schedule arrives already computed rather than being worked out in SQL, so
-- the backoff can be tested against an injected clock instead of against now().
-- name: RecordDomainCheck :execrows
UPDATE domains
SET state = @state,
    observed = @observed,
    last_error = @last_error,
    last_checked_at = @checked_at,
    next_check_at = @next_check_at,
    check_attempts = @check_attempts,
    -- First proof only. This is when the domain was first shown to be ours, and
    -- re-stamping it on every later check would turn it into a duplicate of
    -- last_checked_at.
    verified_at = CASE
        WHEN @state::text IN ('verified', 'routed') AND verified_at IS NULL THEN @checked_at
        ELSE verified_at
    END
WHERE id = @id;

-- Claims the domains whose next check is due, and leases them.
--
-- Two things at once, and both matter.
--
-- FOR UPDATE SKIP LOCKED is what lets the checker run on every replica without
-- those replicas agreeing on anything: each takes rows nobody else holds, and a
-- row already being worked on is skipped rather than waited for. The build
-- reconciler is safe on several replicas for the same reason.
--
-- Pushing next_check_at forward in the same statement is what keeps the lock
-- short. The alternative — holding the transaction open while the lookups run —
-- would hold row locks for as long as DNS takes to answer, which is unbounded
-- and occasionally seconds. Leasing instead means the claim is committed
-- immediately and the network happens outside any transaction; a checker that
-- dies mid-pass simply lets the lease expire, and the next pass picks the row
-- up again.
-- name: ClaimDomainsDueForCheck :many
UPDATE domains SET next_check_at = @lease_until
WHERE id IN (
    SELECT due.id FROM domains due
    WHERE NOT due.managed AND due.next_check_at <= @due_before
    ORDER BY due.next_check_at
    LIMIT @lim
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- Moves a proven domain to routed, once the Ingress actually carries it.
--
-- Guarded on the state it is coming from, so a check that has since found drift
-- is not overwritten by an apply that started before it.
-- name: MarkDomainRouted :execrows
UPDATE domains
SET state = 'routed'
WHERE id = @id AND state = 'verified';

-- A verified domain becomes routed only after the app reconciler has observed
-- or produced the matching workload convergence key. This app-scoped form
-- closes the asynchronous handoff from the DNS checker.
-- name: MarkVerifiedDomainsRoutedForApp :execrows
UPDATE domains
SET state = 'routed'
WHERE owner_id = @owner_id AND app_id = @app_id
  AND NOT managed AND state = 'verified';

-- name: HasVerifiedDomainsForApp :one
SELECT EXISTS (
    SELECT 1 FROM domains
    WHERE owner_id = @owner_id AND app_id = @app_id
      AND NOT managed AND state = 'verified'
);

-- Brings a domain's next check forward. What the "Check now" button does.
--
-- It resets the attempt count as well, because somebody pressing it is telling
-- us the situation changed — and a domain that had backed off to a fifteen
-- minute interval should not go straight back to one.
-- name: RequestDomainCheck :execrows
UPDATE domains
SET next_check_at = @due, check_attempts = 0
WHERE owner_id = @owner_id AND id = @id AND NOT managed;

-- Every custom domain on the install, worst first.
--
-- Nothing answered "which domains are stuck" before this. A domain that is not
-- routed is the only kind anybody needs to find, so it sorts to the top.
-- name: ListCustomDomainsForOwner :many
SELECT sqlc.embed(d), a.name AS app_name
FROM domains d
JOIN apps a ON a.id = d.app_id
WHERE d.owner_id = @owner_id AND NOT d.managed
ORDER BY (d.state = 'routed'), d.host;

-- name: DeleteCustomDomain :execrows
DELETE FROM domains
WHERE owner_id = @owner_id AND id = @id AND NOT managed;

-- name: GetCustomDomain :one
SELECT * FROM domains
WHERE owner_id = @owner_id AND id = @id AND NOT managed;

-- Hostnames that may actually be routed to.
--
-- A managed host is routable because the platform issued it; a custom one only
-- once it is proven. This is the query the Ingress is built from, so the gate
-- lives here rather than in a caller that might forget it.
-- name: RoutableHostsForApp :many
SELECT host FROM domains
WHERE app_id = @app_id AND (managed OR verified)
ORDER BY managed DESC, host;
