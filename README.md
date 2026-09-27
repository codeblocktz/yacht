# Yacht

A self-hosted PaaS for Kubernetes. Deploy your app to your own server, with
real Kubernetes underneath rather than a Docker wrapper, and an all-Go control
plane you can read in an afternoon.

Runs on K3s, so a $5 VPS is enough to start.

> [!WARNING]
> **Not ready for production.** Yacht is under active development. Interfaces,
> the database schema, and behaviour all still change without notice, there has
> been no tagged release yet, and none of it has run anywhere long enough to
> have earned your trust.
>
> Run it on something you can afford to lose, and keep backups you have actually
> restored from. Do not put it in front of anything whose downtime or data loss
> would matter.

**What it does today.** You can deploy from a container image or from a Git
repository, give it a domain, attach storage, and watch it from the dashboard.
See [What works today](#what-works-today).

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
| Change the image, port, limits and repository afterwards | ✅ |
| Build and deploy from a Git repository, using buildpacks and an external registry | ✅ |
| Branches read from the remote as you type | ✅ |
| Deploy on push, from GitHub, GitLab, Gitea or Forgejo webhooks | ✅ |
| Builds run as an isolated Job, with the log kept | ✅ |
| Build output streamed to the page while it runs | ✅ |
| Push build output to an operator-configured OCI registry | ✅ |
| Scale, redeploy, delete | ✅ |
| Liveness and readiness probes | ✅ |
| Deployment history, with a log per deployment | ✅ |
| Roll back to an earlier release, settings and all | ✅ |
| App logs, and per-request HTTP logs you can search and page | ✅ |
| Live logs, streamed as the container writes them | ✅ |
| Persistent volumes, mounted and expandable | ✅ |
| Secrets sealed at rest, kept out of the app record | ✅ |
| Deploy a wired stack from a template in one action | ✅ |
| Apps that sleep when idle and wake on the next request | ✅ |

**Reaching it**

| | |
|---|---|
| A hostname per app, served the moment it starts | ✅ |
| Custom domains people bring, proven continuously in the background | ✅ |
| A domain that stops resolving is noticed and withdrawn | ✅ |
| TLS from one shared wildcard certificate, for platform hostnames | ✅ |
| A certificate of its own for every brought domain, from Let's Encrypt | ✅ |

A brought domain cannot be covered by the platform wildcard, so each one is
issued its own certificate through cert-manager, which the installer sets up.
Issuance is HTTP-01: public port 80 has to reach the cluster, and must not be
redirected to HTTPS cluster-wide. The domain's page shows the certificate
arriving, when it expires, and — if it does not arrive — what to check. An
install without cert-manager serves brought domains over plain HTTP, and says
so on the domain rather than leaving the browser to.

An app can **sleep** when it has had no requests for a while — 30 minutes by
default, never under 5 — set per app or as a team default, and off until
somebody turns it on. It is scaled to zero, its hostnames are routed to the
engine's waker, and the next request wakes it: an API call is held until the
app answers it, a browser sees a small waking-up page that reloads itself.
Idleness is read from Traefik's per-service request counters (its k3s chart
publishes them by default), so nothing sleeps where requests cannot be
counted. The cluster has to reach the waker: set `YACHT_WAKER_ADDR` to this
node's address and a port open to the cluster's pods. A sleeping app's CPU
and memory are sold to others, less a wake reserve the capacity policy keeps
so that a share of the sleepers can wake at once.

The installer defaults to Let's Encrypt's **staging** environment, whose
certificates browsers do not trust. Re-run it with
`--acme-environment production --acme-email you@example.com` once you are ready.

**Running the cluster**

| | |
|---|---|
| Live workload status read from the cluster | ✅ |
| Cluster view — nodes, pods, volumes, events, utilisation | ✅ |
| Per-team quotas on apps, CPU, memory and storage, set by the operator | ✅ |
| Act as a team for support — named operators only, re-checked every request, bannered on every page, recorded | ✅ |
| Capacity — the cluster's room beside what every team has committed | ✅ |
| Add a node, then cordon, drain, or remove one | ✅ |
| [Servers in more than one place](#servers-in-more-than-one-place), with app replicas spread across them | ✅ |
| Namespace provisioning with enforced security posture | ✅ |
| A project canvas apps can be arranged on | ✅ |

**Accounts and foundations**

| | |
|---|---|
| Magic-link sign-in, sessions, sign-out everywhere | ✅ |
| Optional password, added and removed from the account page | ✅ |
| Teams with Owner / Admin / Member and invitations | ✅ |
| Identity seam — single owner, bearer token, or session | ✅ |
| Dashboard with pluggable chrome, light and dark | ✅ |
| Postgres schema + embedded migrations | ✅ |

Utilisation percentages need `metrics-server` in the cluster. Without it
everything else still works and those figures read `—` rather than zero.

## Install on a VPS

> **Not installable yet.** This repository is public and the Pages workflow is
> configured to publish only scripts whose exact revision passed CI. However,
> there is no tagged release yet, so the installer has no release assets to
> download. Do not pipe the URL below into a shell until a release is announced;
> until then, use [Quick start](#quick-start).

```bash
curl -sSL https://codeblocktz.github.io/yacht/install.sh | sudo sh
```

Debian or Ubuntu, amd64 or arm64. It installs K3s, Postgres, and Yacht as a
systemd service, then prints the dashboard URL and a token to sign in with.
About ninety seconds on a fresh box.

To upgrade later:

```bash
curl -sSL https://codeblocktz.github.io/yacht/upgrade.sh | sudo sh
```

That one only swaps the binary and restarts. If the new version does not come
up healthy it puts the old one back, so a bad release costs a restart rather
than an outage.

| | |
|---|---|
| Binary | `/usr/local/bin/yacht` |
| Config | `/etc/yacht/yacht.env` |
| Service | `systemctl status yacht` |
| Logs | `journalctl -u yacht -f` |

**Back up `/etc/yacht/yacht.env`.** It holds `YACHT_SECRET_KEY`, which seals
your stored secrets and cannot be regenerated — losing it loses them. Re-running
the installer preserves it, and every other generated value, on purpose.

The installer serves the dashboard over plain HTTP, so the token crosses the
network in the clear. Set `YACHT_BASE_URL` and `YACHT_APP_DOMAIN` in the config
and put it behind TLS before you rely on it; until then `ssh -L 8080:127.0.0.1:8080`
is the safer way in.

Both scripts are worth reading before you pipe them to a root shell —
[`install.sh`](install.sh) and [`upgrade.sh`](upgrade.sh) are exactly what those
URLs serve, published straight from this repository.

To remove it all:

```bash
sudo systemctl disable --now yacht
sudo rm -rf /etc/yacht /etc/systemd/system/yacht.service /usr/local/bin/yacht
sudo /usr/local/bin/k3s-uninstall.sh          # only if you want the cluster gone too
sudo -u postgres dropdb yacht && sudo -u postgres dropuser yacht
```

## Servers in more than one place

One install can run on machines from different hosting companies, or
different locations of one, and apps keep running across them without their
owners knowing or caring where.

**A site** is a short name you give a machine when it joins, for where it is:
`dar-a`, `dar-b`. Use one name per place. Yacht stores it in the standard
`topology.kubernetes.io/zone` label, so Kubernetes understands it without
anything extra. A machine with no site is simply a machine; an install in
one place needs none and nothing about it changes.

**Install the server with `--multi-site`.** It has to be decided when the
server is first installed:

```bash
curl -sSL https://codeblocktz.github.io/yacht/install.sh | sudo sh -s -- --multi-site --site dar-a
```

Pod traffic between machines then goes over WireGuard, so it is encrypted
wherever it crosses. K3s' default network assumes one private network and
sends that traffic in the clear. The server's public address is taken from
its default route. If that address is not on one of its interfaces, as behind
NAT or a floating IP, pass it with `--public-ip 203.0.113.10`. A private address
is refused rather than guessed. Every machine needs the kernel's WireGuard,
which any current Debian or Ubuntu has.

The network backend is fixed for the life of a cluster. On a server where K3s
already runs with the default backend, `--multi-site` refuses and explains why
instead of reconfiguring it. Changing it means restarting K3s on every node,
and pods on different machines can't reach each other until that's done.

**Join a machine at another site** from Admin → Nodes → Add node. Set the
server address to the server's **public** address
(`https://203.0.113.10:6443`), fill in the site, and fill in the public address
only if the new machine's is not on its own interface. Then run the command
it gives you.

**Open these ports** between sites. The numbers are
[K3s'](https://docs.k3s.io/installation/requirements#inbound-rules-for-k3s-nodes):

| Port | From | To | For |
|---|---|---|---|
| 6443/tcp | every machine | the server | joining, and the Kubernetes API |
| 51820/udp | every machine | every machine | WireGuard |
| 51821/udp | every machine | every machine | WireGuard over IPv6 |
| 10250/tcp | every machine | every machine | the kubelet: logs, exec, metrics |

**Replicas spread out.** An app with more than one replica prefers to have
them on different machines, and on different sites once any machine has one.
Losing one machine, or everything in one place, then takes some replicas
rather than all of them. These are preferences, not rules. A full site, or an
install on one machine, still runs every replica. Once machines are in more
than one place, give every machine a site, including the first server:
Kubernetes leaves a machine with no site out of spreading by site. Label one
that joined without a site with
`kubectl label node <name> topology.kubernetes.io/zone=<site>`.

What this doesn't do yet:

- **A volume lives on one machine.** An app with a volume runs where its
  volume is and doesn't move to another site. Moving volumes between machines
  is a later piece of work.
- **Traffic enters where DNS points.** Requests reach the cluster at the
  machines your DNS names, and cross over WireGuard to wherever the app runs.
  Entry points at more than one site come later.
- **One server.** The control plane is the one machine you installed first.
  If it goes, running apps keep running, but nothing new deploys until it
  is back. A control plane spread across sites comes later.

## Quick start

For development, or to run against a cluster you already have.

Requirements: Go 1.26.6+, Postgres, and a kubeconfig pointing at a cluster.

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

Deploying an existing container image needs no registry configuration. Building
from a Git repository does: Yacht does **not** run a registry, so first provide
an external OCI registry and push credential under **Admin → Registry**. For
private registries, a token service on an unrelated registrable domain is
untested and unsupported in this main-based version; current main does not
proactively reject that topology. The manifest-resolution integration will
enforce the stricter boundary: token services use the registry authority for
plain HTTP, or the same registrable domain over TLS. Every K3s node must also be
configured separately to pull from an insecure registry.

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
| `YACHT_OPERATORS` | — | Emails of the people who run the install — the Admin area: teams and their quotas, capacity, nodes, cross-team views, registry, DNS. Empty means each team's owner — set it once an install hosts several teams. Only named operators may act as a team for support; taking an address off the list ends that person's impersonation on their next request |
| `YACHT_CERT_ISSUER` | — | cert-manager ClusterIssuer that gives each custom domain its own certificate. The installer sets `yacht-acme` |
| `YACHT_BASE_URL` | — | Public URL. **Setting it switches sign-in on** |
| `YACHT_SMTP_ADDR` / `YACHT_RESEND_API_KEY` | — | How sign-in links are delivered. Neither means they go to the log |
| `YACHT_WAKER_ADDR` | — | `ip:port` the cluster reaches the waker at. **Setting it lets idle apps sleep.** The waker listens on its own port, never the dashboard's |
| `YACHT_WAKE_TIMEOUT` | `60s` | How long a wake may take before the app goes back to sleep |
| `YACHT_DEBUG` | `false` | Verbose logging |

The full list, with the reasoning behind each, is in
[`.env.example`](.env.example).

## Wrapping the engine

Yacht is an engine with four seams — orchestrator, identity, dashboard chrome,
notifications — and package [`engine`](engine) is its public surface. An
application built on it composes the same engine with overrides rather than a
fork:

```go
cfg, _ := engine.LoadConfig()
engine.Run(ctx, cfg, engine.Overrides{
	Slots:    myChrome{},              // brand, header tools, banner, extra nav
	Identity: myOrganisations,         // who a request acts as
	Extra:    engine.ExtraRoutes{Owner: mountBilling},
})
```

Everything commercial belongs in the wrapper: `make verify` refuses a tenant,
subscription, invoice or wallet declared in the engine.

Two things in the chrome are worth knowing. `Slots.BrandMark` replaces Yacht's
mark beside your `BrandName` (`templ.NopComponent` for a wordmark alone). And
while an operator is acting as a team, the engine draws its acting notice above
whatever `Slots.Banner` you set — it composes with your banner and cannot be
replaced by it. `Surfaces.ActingAs` says whether a request is acting, for
deciding what else to show. A wrapper that builds its own identity on
`Accounts.Provider` passes `engine.OperatorCheck(cfg.Operators)` to
`WithActingCheck` to keep impersonation; without it, no session acts as
anything.

Sleeping is driven through `Engine.Apps` like quotas are:
`SetTeamSleepDefault(ctx, team, engine.SleepPolicy{Enabled: true, After: 30 * time.Minute})`
turns it on for a plan's teams, `SetSleepSetting` and `SleepStatus` override
and read one app, and `SleepNow` / `Wake` do it by hand. For metering, an app
asleep has `App.RunningReplicas()` of zero, and
`SleepIntervals(ctx, team, from, to)` with `engine.AwakeWithin` says how long
each app was awake in a window — charge replicas × awake time. `Serve` runs
the waker beside the dashboard; a wrapper serving its own handler serves
`Engine.Waker()` on `Config.WakerListenAddr()` too, and
`Overrides.WakerBrand` names it on the waking-up page.

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

### Signing in

An emailed link is how everybody gets in first, and it never stops working. A
password is optional and additive: it is added from **Account**, and from then
on either one opens the same session.

- Stored only as an Argon2id hash with a per-row random salt, with the cost
  parameters inside the stored string — so raising them later re-hashes people
  as they sign in rather than resetting anybody.
- A minimum of 12 characters and no composition rules. Every such rule is a
  constraint an attacker subtracts from the search space, and in practice they
  produce a shorter password with a digit on the end.
- Sign-in never reveals whether an address is registered or whether it has a
  password. An unknown address, an address with no password, and a wrong
  password produce the same message, the same status and the same cost.
- Five attempts per address and twenty per client every fifteen minutes,
  counted separately from the link's own budget — so guessing at somebody's
  password can never lock them out of the link they rely on. There is **no
  account lockout**: on a known address that is a denial of service, and the
  most valuable address on any install is the one in the operator's `.env`.
- Adding, changing or removing a password needs either the current password or
  a sign-in from the last ten minutes. Changing or removing one signs out every
  other browser; adding a first one signs out nothing.
- Removing it is allowed, because the emailed link never stopped working.

**Forgotten it?** Ask for a sign-in link, follow it, and set a new one from
Account. There is no separate reset link, deliberately: a magic link already
goes to the same mailbox and grants strictly more, so a second kind of token
would add a table, four routes and four more expiry rules without adding any
security.

No password is ever logged, and none is rendered back into a page.

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
internal/registry     external image-registry settings and credentials
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
make check      # vet + database-backed race tests
make verify     # every local CI/release gate, including the gallery
make build
make dev        # rebuild and run
```

`make check` requires `YACHT_TEST_DATABASE_URL` so owner-scoping and other
database security tests cannot silently skip. For an intentionally partial run,
opt out explicitly with `YACHT_ALLOW_DATABASE_TEST_SKIPS=1 make check`.

`templ` and `sqlc` are Go tool dependencies, and `make css` downloads the
**Tailwind standalone CLI** — a single binary, no Node or npm. A full
`make verify` also requires PostgreSQL, `YACHT_TEST_DATABASE_URL`, and
`shellcheck`.

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
customer does. The generated gallery can be reviewed directly from its output
directory.

Tests use the `client-go` fake clientset, so the full orchestration path —
namespaces, security context, apply idempotency, status — is verified without
a cluster.

## Contributing

Issues and pull requests are welcome. There is no CLA; contributions are
licensed under MIT, the same as the project.

[CONTRIBUTING.md](CONTRIBUTING.md) covers getting set up, what CI enforces, and
the conventions this codebase holds to. One thing worth knowing before your
first test run: database-backed tests skip themselves without a DSN, so
`make check` refuses to start until `YACHT_TEST_DATABASE_URL` is set or you
explicitly opt in to a partial run.

Found a vulnerability? Please do not open an issue — see
[SECURITY.md](SECURITY.md). Participation is covered by the
[Code of Conduct](CODE_OF_CONDUCT.md).

## License

MIT — see [LICENSE](LICENSE).
