# Feature coverage

What Yacht covers, what is deferred, and one thing it deliberately refuses to
build. Compared against Sailbox, since that is the closest K3s-based
self-hosted PaaS and the reference the navigation was measured against.

Kept in the repo rather than in someone's head so a deferred decision stays a
decision instead of quietly becoming an oversight.

---

## Covered

| | Notes |
|---|---|
| **Overview** | Cluster capacity, app count, recent workloads |
| **Apps** | Deploy from image, scale, redeploy, delete, live status |
| **App detail** | Tabbed: Deployments · Variables · Metrics · Settings. Left rail switches apps without returning to the index |
| **Deployments** | Cross-app activity feed with trigger and relative time |
| **Nodes** | Capacity, live utilisation, roles, pools, cordoned state |
| **Pods** | Ready counts, restarts, node placement, phase |
| **Volumes** | Claims with size, class, access modes, bind status |
| **Events** | Cluster events with warnings hoisted above routine chatter |
| **Settings** | Owner, auth posture, cluster reachability, version |

Utilisation figures need `metrics-server`. Without it everything else works and
those numbers read `—` rather than a misleading zero.

---

## Deferred, in rough order of value

### Build from a Git repository
The largest remaining gap and the next milestone. Today an app is deployed from
an image reference; the build pipeline (clone → BuildKit → push to the
authenticated registry → deploy) is designed but not built. Everything
downstream of it — commit messages in deployment history, "deployed via GitHub",
branch tracking — waits on this.

### Ingress, TLS, custom domains
The `domains` table exists in the schema. What is missing is cert-manager
wiring: a wildcard certificate via DNS-01 for platform subdomains, and CNAME
delegation for customer apex domains. Note the constraint that shapes the
design: **Let's Encrypt allows 50 certificates per registered domain per week**,
so per-app subdomains must come from one wildcard, not a certificate each.

### Live log streaming
The dashboard already uses SSE, so the transport is not the work — following a
rolling pod set as replicas come and go is.

### Managed databases
Roughly a third of a full PaaS's orchestration surface. When it lands it should
be **CloudNativePG**, an operator that already handles failover, backups and
point-in-time recovery — not hand-rolled StatefulSets. Until then, customers
point at Neon, Supabase, or a Postgres run by hand.

### Cron jobs
Real orchestration work: `CronJob` objects, run history, manual trigger, log
retrieval. Not a page, a subsystem.

### Projects
Grouping apps under projects. Worth doing once an install has enough apps to
need it, and worth doing carefully: in a wrapping multi-tenant application a
project is the natural placement unit, so the schema should anticipate that
rather than be retrofitted around it.

### Shared resources
External registry credentials, object storage for backups, SSH keys. Needed
before builds and database backups are useful, so it lands alongside them.

### Web terminal
Deliberately late. Exec-over-websockets is fiddly, and it is the single most
dangerous endpoint a multi-tenant control plane can expose. Logs answer most of
the questions a shell would.

### Deferred small items
Helm release listing, DaemonSet inspection, node topology visualisation,
namespace browser, PVC expansion, cluster garbage collection, metric history
snapshots, alert rules, notification channels, 2FA.

---

## Deliberately refused: editing the ingress controller's configuration

Sailbox exposes a Traefik configuration editor. Yacht will not, and this is a
security position rather than a scheduling one.

That endpoint writes an operator-supplied spec into the `traefik` HelmChart
custom resource in `kube-system`. K3s's helm-controller then renders and applies
it with cluster-admin-grade rights. In Sailbox the route carries no role gate at
all, so **the lowest-privilege account the product offers can take over the
cluster** — via a documented feature, with no memory-safety bug involved.

The underlying problem is not the missing role check. It is that "let a user
supply arbitrary ingress-controller configuration" and "contain that user"
cannot both be true. A role gate narrows who can do it; it does not make the
capability safe, and any product that later grows tenants inherits the hole.

So Yacht treats Traefik as infrastructure it configures, never as configuration
it accepts. Where ingress behaviour genuinely needs to vary per app — headers,
redirects, rate limits — the answer is a narrow, validated field on the app,
not a passthrough to the cluster.

The same reasoning covers a handful of Sailbox capabilities that are not on the
deferred list because they should not be built: cluster-wide PVC deletion by
path, arbitrary key/value writes to global settings, and cleanup jobs that
sweep namespaces the engine does not own.

---

## How to check this document is still true

Navigation and routes drift apart quietly. `TestEveryNavTargetResolves` walks
every advertised nav item and fails if any of them 404s, so a page promised in
the sidebar cannot silently not exist.

For the visual side, `make gallery` renders every state — degraded workloads,
failed deploys, a node at 98%, an unreachable cluster — without needing a
cluster or a database.
