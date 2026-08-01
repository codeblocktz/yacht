# Persistent volumes

**Date:** 2026-08-01
**Status:** approved, not yet implemented
**Scope:** sub-project D

---

## The problem

Yacht can deploy a workload and give it a URL, but not a place to keep
anything. `AppSpec` has no volume field, nothing creates a
`PersistentVolumeClaim`, and `/cluster/volumes` is a listing of claims the
engine never made.

So every app is ephemeral. A database, a queue, an app that writes uploads —
none of them can run. That is a large share of what people actually deploy.

---

## What this delivers

Per-app named volumes: a size, a mount path, and a claim that survives
redeploys. Plus expansion, and a storage tab that shows what an app has.

**Not** in scope: volumes shared between apps, snapshots, backups, or object
storage. Each is its own sub-project, and the first of them changes the
ownership model rather than extending it.

---

## The constraint everything else follows from

A `ReadWriteOnce` volume can be mounted by **one node at a time**. K3s ships
`local-path`, which is RWO-only, and that is what the design must assume.

Two consequences, neither avoidable in code:

### Rolling updates deadlock

The Deployment currently uses Kubernetes' default `RollingUpdate`: a new pod
starts before the old one terminates. With a volume attached, the new pod
cannot mount what the old pod still holds, so it stays `Pending` and the deploy
never finishes. Nothing times out; it simply hangs.

**Attaching a volume switches the workload to `strategy: Recreate`.** The old
pod stops, then the new one starts. That means **a deploy of an app with
storage has downtime**, and the dashboard must say so rather than change deploy
behaviour silently.

### More than one replica is impossible

The second pod can never schedule — there is nothing to schedule it onto that
can also mount the volume. `AppSpec.Validate` rejects volumes with
`Replicas > 1`, and the form says a stored app runs one replica.

Refusing at validation rather than letting Kubernetes decide is the same
judgement made elsewhere in this codebase: a pod stuck `Pending` forever, with
the reason buried in `kubectl describe`, is a worse answer than a refusal at
the moment somebody asks.

---

## Data model

Migration `00006_volumes.sql`:

```sql
CREATE TABLE volumes (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id   text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    app_id     uuid NOT NULL REFERENCES apps (id) ON DELETE CASCADE,

    name       text NOT NULL CHECK (name ~ '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'
                                    AND length(name) <= 40),
    mount_path text NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes > 0),
    class      text NOT NULL DEFAULT '',

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX volumes_app_name_key ON volumes (app_id, name);
CREATE UNIQUE INDEX volumes_app_mount_key ON volumes (app_id, mount_path);
CREATE INDEX volumes_owner_id_idx ON volumes (owner_id);
```

`owner_id` is present because every resource table carries it — the invariant
`TestEveryTableIsOwnerScoped` enforces. Two unique indexes, both scoped by app:
one name per app, and one claim per mount path. Mounting two volumes at the
same path is a workload where one silently wins.

Size is stored in bytes rather than as a Kubernetes quantity string, so
comparing an old size to a new one is arithmetic rather than parsing.

---

## Expansion only

Kubernetes cannot shrink a `PersistentVolumeClaim`. Nothing can: the filesystem
on it may be full.

So resize is expansion, refused below the current size with a message saying
why. The storage class must also have `allowVolumeExpansion: true`; where it
does not, the API server rejects the update and the error is surfaced rather
than swallowed.

The engine does not attempt the alternative — create a new claim, copy, swap —
because a copy that fails halfway is data loss, and the honest answer to
"make this smaller" is that it cannot be done in place.

---

## Deletion

A volume attached to an app is refused, with the app named. Detaching is an
edit of the app; deleting is a separate act.

Deleting the app deletes its volumes, because the `apps` row cascades and the
namespace goes with it. That is the one path where storage disappears without a
separate confirmation, and the delete form says so.

---

## Scoping

Claims carry `yacht/owner-id` like everything else the engine manages, so the
team scoping already applied to `Volumes(ctx, owner)` covers them with no
further work. This is the payoff for fixing that leak before building here
rather than after.

---

## Testing

**Validation** — a volume with `Replicas > 1` is refused; mount path must be
absolute, must not be `/`, and must not collide with another volume on the same
app; size must be positive.

**Orchestrator (fake clientset)** — a PVC is created in the app's namespace with
the owner label; the container gets a matching `volumeMount`; the Deployment
strategy is `Recreate` when volumes are attached and left default when they are
not; removing the last volume returns the strategy to rolling; `DeleteApp`
leaves the claim to the namespace rather than deleting it twice.

**Expansion** — growing updates the claim; shrinking is refused before anything
is sent to the cluster.

**Store** — the two unique indexes hold; `TestEveryTableIsOwnerScoped` passes
with the new table.

**Live** — the check that matters, and the one no fake can make: deploy an app
with a volume on k3d, write a file into the mount, redeploy, and read the file
back. That is the whole feature in one assertion.

---

## Relationship to other work

- **A** — per-app hostnames. Done, proven on a cluster.
- **B, C** — accounts, teams, sign-in. Done.
- **Cluster-view scoping** — done immediately before this, and a prerequisite:
  building storage on an unscoped listing would have extended a disclosure.
- **D — this document.**

## Deferred

Volumes shared between apps, snapshots, scheduled backups, object storage, and
`ReadWriteMany`. The last is untestable on `local-path`, and shipping a code
path that has never run is the failure mode this project keeps finding.
