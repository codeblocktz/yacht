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
| **Per-app hostnames** | `<name>.<YACHT_APP_DOMAIN>`, issued at create time in the same transaction as the app |
| **TLS** | One shared wildcard, served by the ingress controller's default certificate |
| **Persistent storage** | Per-app volumes, mounted, kept across redeploys, expandable |
| **Accounts** | Magic-link sign-in, sessions, sign-out everywhere |
| **Teams** | Owner / Admin / Member, invitations, team switcher, member management |
| **Deployments** | Cross-app activity feed with trigger and relative time |
| **Nodes** | Capacity, live utilisation, roles, pools, cordoned state |
| **Pods** | Ready counts, restarts, node placement, phase — scoped to your team |
| **Volumes** | Claims with size, class, access modes, bind status — scoped to your team |
| **Events** | Cluster events with warnings hoisted above routine chatter |
| **Settings** | Owner, auth and sign-in posture, mail transport, hostname policy, cluster reachability, version |

Utilisation figures need `metrics-server`. Without it everything else works and
those numbers read `—` rather than a misleading zero.

Two consequences worth stating, because they surprise people:

- **Images that run as root will not start.** Every namespace is Pod Security
  Admission `restricted` and there is no API for relaxing it.
- **An app with storage runs one replica and recreates on deploy**, so it has
  brief downtime. A `ReadWriteOnce` claim mounts on one node at a time: a second
  pod has nowhere to schedule, and a rolling update deadlocks waiting for a
  volume the outgoing pod still holds.

---

## Deferred, in rough order of value

### Build from a Git repository
The largest remaining gap and the next milestone. Today an app is deployed from
an image reference; the build pipeline (clone → BuildKit → push to the
authenticated registry → deploy) is designed but not built. Everything
downstream of it — commit messages in deployment history, "deployed via GitHub",
branch tracking — waits on this.

### Customer custom domains
Platform hostnames and their shared wildcard certificate are built. What is
missing is the other half: letting a customer point their own domain at an app —
claim, prove by DNS TXT, publish, and a cert-manager `Certificate` per host.

The constraint that shaped the built half still shapes this one: **Let's Encrypt
allows 50 certificates per registered domain per week**, which is why platform
subdomains come from one wildcard. Customer domains are different registered
domains, so the limit does not aggregate and one certificate per host is right
there.

`domain.Reserved` already exists and is tested — it refuses any claim under the
platform domain, which is the piece that is unsafe to retrofit once tenants can
claim names.

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

### Secrets
`AppSpec.Env` is a plain map stored as `jsonb` and rendered straight into the
Deployment. A password pasted into it is plaintext in Postgres, in every
backup, and in `kubectl get deploy -o yaml` — and nothing on the form says so.
Splitting secret from non-secret, and mounting the secret half with `envFrom`,
is the highest-value item on this list.

### Health probes
Liveness and readiness on `AppSpec`. More pressing since storage landed: an app
with a volume recreates on deploy rather than rolling, so there is a real gap
between the old pod stopping and the new one serving, and without a readiness
probe traffic arrives before the container is up.

### Deferred small items
Helm release listing, DaemonSet inspection, node topology visualisation,
namespace browser, cluster garbage collection, metric history snapshots, alert
rules, notification channels beyond email, 2FA enrolment (the columns exist),
and a cap on outstanding magic links.

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
