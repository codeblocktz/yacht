# Yacht

A self-hosted PaaS for Kubernetes. Deploy your app to your own server, with
real Kubernetes underneath rather than a Docker wrapper, and an all-Go control
plane you can read in an afternoon.

Runs on K3s, so a $5 VPS is enough to start.

> **Status: early, but usable.** You can deploy from a container image or from
> a Git repository, give it a domain, attach storage, and watch it from the
> dashboard. See [What works today](#what-works-today).

## Why

Existing self-hosted PaaS options mostly wrap Docker Compose or Swarm. That
works until you want health probes, rolling updates, or a second node — at
which point you are fighting the abstraction instead of shipping.

Kubernetes already solves those problems. K3s makes it small enough to run on a
single cheap box. Yacht is a control plane over it that stays out of the way:
every workload is an ordinary Deployment, so `kubectl` keeps working and
nothing you build here is locked in.

## What works today

**Shipping an app**

| | |
|---|---|
| Deploy a container image, with env vars and replicas | ✅ |
| Build and deploy from a Git repository, using buildpacks | ✅ |
| Builds run as an isolated Job, with the log kept | ✅ |
| A built-in registry for the images those builds produce | ✅ |
| Scale, redeploy, delete | ✅ |
| Liveness and readiness probes | ✅ |
| Deployment history, with a log per deployment | ✅ |
| App logs, and per-request HTTP logs you can search and page | ✅ |
| Persistent volumes, mounted and expandable | ✅ |
| Secrets sealed at rest, kept out of the app record | ✅ |
| Deploy a wired stack from a template in one action | ✅ |

**Reaching it**

| | |
|---|---|
| A hostname per app, served the moment it starts | ✅ |
| Custom domains people bring, with verification | ✅ |
| TLS from one shared wildcard certificate | ✅ |

**Running the cluster**

| | |
|---|---|
| Live workload status read from the cluster | ✅ |
| Cluster view — nodes, pods, volumes, events, utilisation | ✅ |
| Add a node, then cordon, drain, or remove one | ✅ |
| Namespace provisioning with enforced security posture | ✅ |
| A project canvas apps can be arranged on | ✅ |

**Accounts and foundations**

| | |
|---|---|
| Magic-link sign-in, sessions, sign-out everywhere | ✅ |
| Teams with Owner / Admin / Member and invitations | ✅ |
| Identity seam — single owner, bearer token, or session | ✅ |
| Dashboard with pluggable chrome, light and dark | ✅ |
| Postgres schema + embedded migrations | ✅ |
| Logs stream rather than refresh on a 5s poll | next |

Utilisation percentages need `metrics-server` in the cluster. Without it
everything else still works and those figures read `—` rather than zero.

## Quick start

Requirements: Go 1.26+, Postgres, and a kubeconfig pointing at a cluster.

```bash
git clone https://github.com/codeblocktz/yacht.git
cd yacht

export YACHT_DATABASE_URL="postgres://yacht:yacht@localhost:5432/yacht?sslmode=disable"
export YACHT_KUBECONFIG="$HOME/.kube/config"
export YACHT_AUTH_TOKEN="$(openssl rand -hex 24)"   # omit only on a trusted network

make run
```

Then open <http://localhost:8080>.

Migrations run automatically at startup. If the cluster is unreachable Yacht
still boots and says so on the overview page, so you can fix your kubeconfig
without digging through logs.

### Configuration

| Variable | Default | Notes |
|---|---|---|
| `YACHT_DATABASE_URL` | — | **Required.** Postgres connection string |
| `YACHT_ADDR` | `:8080` | Listen address |
| `YACHT_KUBECONFIG` | `$KUBECONFIG` | Path to a kubeconfig |
| `YACHT_KUBE_IN_CLUSTER` | `false` | Use the mounted service account instead |
| `YACHT_AUTH_TOKEN` | — | Bearer token. Unset, and with no accounts, means **no authentication** |
| `YACHT_OWNER_ID` | `owner-local` | The team every resource belongs to on a fresh install |
| `YACHT_OWNER_EMAIL` | — | The one address that may sign in before anybody has an account |
| `YACHT_APP_DOMAIN` | — | Apps get `<name>.<this>`. Point `*.<this>` at the cluster |
| `YACHT_WILDCARD_TLS` | `false` | Serve those hostnames from the controller's default certificate |
| `YACHT_BASE_URL` | — | Public URL. **Setting it switches sign-in on** |
| `YACHT_SMTP_ADDR` / `YACHT_RESEND_API_KEY` | — | How sign-in links are delivered. Neither means they go to the log |
| `YACHT_DEBUG` | `false` | Verbose logging |

The full list, with the reasoning behind each, is in
[`.env.example`](.env.example).

## Security posture

Workloads are hardened by construction, not by configuration. Every namespace
Yacht creates is labelled for Pod Security Admission at `restricted` and gets a
default `LimitRange`; every pod runs with:

- `runAsNonRoot`, `allowPrivilegeEscalation: false`, `privileged: false`
- all Linux capabilities dropped
- `seccompProfile: RuntimeDefault`
- a read-only root filesystem, with a writable `/tmp` so that stays practical
- no service account token mounted

There is no API for privileged containers, host networking, or host paths —
not as an omission to be filled in later, but because a request for them has
nowhere to go. Images that genuinely need a writable root filesystem have one
explicit, visible escape hatch (`WritableRootFilesystem`).

An important consequence: **images that run as root will not start.** That is
the intended behaviour. Most official images already ship a non-root user.

## Architecture

Yacht is built to be wrapped. Anything that needs to differ for a hosted,
multi-tenant deployment goes through one of three seams, so a larger
application can build on this module rather than fork it:

| Seam | Interface | Engine ships | A wrapper supplies |
|---|---|---|---|
| Orchestration | `orchestrator.Orchestrator` | single cluster | multi-cluster placement |
| Identity | `identity.Provider` | single owner, bearer token | tokens resolved to an account |
| Dashboard chrome | `web.SlotProvider` | plain navigation | account switcher, usage, billing |

Two rules keep the seams honest:

1. **No Kubernetes types cross the orchestrator boundary.** Callers never
   import `client-go`, and a non-Kubernetes backend stays possible.
2. **Every table carries `owner_id`, and unique constraints are scoped by it.**
   The engine writes one value there forever. It exists so that scoping is a
   cheap indexed predicate rather than a join added later — a predicate is a
   check that gets written, a join is a check that gets skipped.

```
cmd/yacht             entrypoint and wiring
internal/app          workload lifecycle — keeps database and cluster agreeing
internal/account      people: users, teams, roles, invitations
internal/cluster      how a machine joins this cluster
internal/config       environment configuration
internal/domain       hostnames — the one we issue, and the ones people bring
internal/notify       delivers messages to people
internal/registry     where images this install builds are pushed
internal/secret       values that must survive a database dump
internal/identity     SEAM 2 — who owns this request
internal/orchestrator SEAM 1 — the runtime contract
          └── k8s     Kubernetes implementation
internal/store        schema, embedded migrations, sqlc queries
internal/web          SEAM 3 — dashboard, slot-based layout
```

The database is the source of truth for which apps exist; the cluster is the
source of truth for how they are doing. Neither is asked the other's question,
which is why listing apps never enumerates Deployments and why status is never
cached in a column that can go stale.

## Development

```bash
make assets     # templ codegen + Tailwind
make check      # vet + tests
make build
make dev        # rebuild and run
```

`templ` and `sqlc` are Go tool dependencies, and `make css` downloads the
**Tailwind standalone CLI** — a single binary, no Node or npm. So a contributor
needs nothing installed beyond Go.

Generated `*_templ.go` files and the compiled `app.css` are both committed,
which keeps plain `go build` working for anyone who has not run codegen. CI
rebuilds both and fails on drift.

### Design system

The UI uses the [templUI](https://templui.io) / shadcn token set — the same
CSS custom properties, so a component copied in with `templui add <name>`
inherits this theme unchanged. Deliberate departures:

- **Monochrome primary.** A saturated primary button reads as consumer
  software. Colour is reserved for state, where it carries information.
- **Denser than stock.** 13px base, tighter rows, borders instead of shadows,
  a metric strip instead of a grid of stat cards.
- **Status is a dot and a word**, not a filled pill. A page of coloured pills
  is noise, and the one that matters stops standing out.

Light and dark both ship, resolved before first paint so there is no flash on
navigation.

### Visual states

Every state the dashboard can be in — degraded workloads, failed deploys, a
node at 98%, an unreachable cluster — renders from a gallery, without needing a
cluster or a database:

```bash
YACHT_GALLERY_OUT=/tmp/gallery go test ./internal/web -run Gallery
```

Those are the states that silently rot, because nobody sees them until a
customer does. Screenshots in `docs/images/`.

Tests use the `client-go` fake clientset, so the full orchestration path —
namespaces, security context, apply idempotency, status — is verified without
a cluster.

## Contributing

Issues and pull requests are welcome. There is no CLA; contributions are
licensed under MIT, the same as the project.

## License

MIT — see [LICENSE](LICENSE).
