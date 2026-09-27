-- The team's projects and apps as the dashboard's chrome draws them: the
-- sidebar's project tree, the command palette's index, the deploys-in-flight
-- count and the getting-started checklist.
--
-- The chrome renders on every page, so everything here is shaped to be read
-- from the database in a fixed number of queries whatever the team's size. In
-- particular nothing asks the cluster: an app's state is what its records say
-- — a live operation, a release serving, the last attempt failing — rather
-- than what its pods say, which would cost a round trip to the cluster per app
-- per page. The app's own page and its canvas still ask the cluster.
--
-- Read-only on purpose. Projects lists the default project into existence
-- when a team has none; a sidebar that did that would create a project on the
-- first page a new team ever loads.

-- name: WorkspaceProjects :many
SELECT id, slug, name
FROM projects
WHERE owner_id = @owner_id
ORDER BY name;

-- One row per app, with what it takes to say how the app stands without
-- asking the cluster. deployment_operations holds at most one live row per app
-- (see deployment_operations_one_live_per_app), so the EXISTS is one index
-- probe.
-- name: WorkspaceApps :many
SELECT a.name, a.project_id, a.replicas,
       (a.active_release_id IS NOT NULL)::boolean AS serving,
       COALESCE(d.status, '')::text AS last_deploy,
       EXISTS (
           SELECT 1 FROM deployment_operations o
           WHERE o.owner_id = a.owner_id AND o.app_id = a.id
             AND o.status IN ('queued', 'claimed', 'building', 'applying', 'verifying')
       ) AS deploying
FROM apps a
LEFT JOIN LATERAL (
    SELECT status FROM deployments
    WHERE app_id = a.id
    ORDER BY started_at DESC
    LIMIT 1
) d ON true
WHERE a.owner_id = @owner_id
ORDER BY a.name;

-- The facts the checklist and the Deployments badge are computed from.
--
-- A deploy counts as having succeeded if it ever did: superseded and active
-- are older rows' words for the same outcome. A custom domain counts once it
-- is routed — claimed but unverified is a step started, not a step done.
-- name: WorkspaceFacts :one
SELECT
    (SELECT count(*) FROM deployment_operations o
     WHERE o.owner_id = @owner_id
       AND o.status IN ('queued', 'claimed', 'building', 'applying', 'verifying')
    )::bigint AS live_deploys,
    EXISTS (SELECT 1 FROM apps a WHERE a.owner_id = @owner_id) AS has_apps,
    EXISTS (
        SELECT 1 FROM deployments d
        WHERE d.owner_id = @owner_id
          AND d.status IN ('succeeded', 'active', 'superseded')
    ) AS has_succeeded_deploy,
    EXISTS (
        SELECT 1 FROM domains m
        WHERE m.owner_id = @owner_id AND NOT m.managed AND m.state = 'routed'
    ) AS has_custom_domain,
    EXISTS (
        SELECT 1 FROM teams t
        WHERE t.id = @owner_id AND t.onboarding_dismissed_at IS NOT NULL
    ) AS onboarding_dismissed;

-- Puts the checklist away for the whole team.
--
-- An upsert because a single-owner install has no team row until its first
-- app is created, and dismissing the checklist before deploying anything is a
-- reasonable thing to do. The row it creates is the one CreateTeamRow fills in
-- later.
-- name: DismissOnboarding :exec
INSERT INTO teams (id, onboarding_dismissed_at)
VALUES (@owner_id, now())
ON CONFLICT (id) DO UPDATE SET onboarding_dismissed_at = now();
