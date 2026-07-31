# Per-app hostnames and shared wildcard TLS

**Date:** 2026-07-31
**Status:** approved, not yet implemented
**Scope:** sub-project A of three (see "Relationship to other work")

---

## The problem

An app deployed through Yacht today is unreachable. `ApplyApp` creates a
Deployment and, when a port is set, a ClusterIP Service — and nothing else.
ClusterIP is cluster-internal. There is no Ingress anywhere in the codebase;
searching for `ingress`, `networking.k8s.io` or `networkingv1` across all Go,
templ and SQL sources returns two comments and no code.

So the core loop is broken: deploy an app, watch it start, and then have no way
to visit it. Everything else in the product is downstream of fixing that.

The `domains` table already exists, with indexes and a comment explaining why
its uniqueness is correctly global. Nothing reads or writes it.

---

## What this delivers

Set `YACHT_APP_DOMAIN=apps.example.com`, point a wildcard DNS record at the
cluster, and an app named `web` is reachable at `web.apps.example.com` from the
moment it starts. No per-app DNS step, no manual domain configuration.

The hostname is issued **at create time, not on request** — an app cannot exist
without its URL.

Alongside it, TLS for those hostnames from a single shared wildcard certificate
rather than one certificate per app.

Explicitly **not** in scope: customer custom domains (claim → prove by DNS TXT →
publish), cert-manager, and per-host certificate status. Those are a separate
sub-project. See "Deferred".

---

## Why a shared wildcard, and why not a Secret reference

Let's Encrypt allows 50 certificates per registered domain per week. A platform
handing out one subdomain per app stops being able to issue after app #50, and
the failure arrives as a rate-limit error against a domain nobody touched —
which is close to undiagnosable if you are looking at the app that failed.

One wildcard certificate for the platform domain removes the limit entirely.
Customer domains, when they arrive, stay one certificate per host, because they
are different registered domains and the limit does not aggregate across them.

### The namespace constraint

The obvious design — `YACHT_WILDCARD_TLS_SECRET` naming a pre-provisioned
Secret, referenced from each app's Ingress — does not work, and it is worth
recording why so it is not re-proposed.

An Ingress's `spec.tls[].secretName` must name a Secret in the **Ingress's own
namespace**, and an Ingress's backend Service must also be in that namespace.
Every Yacht app gets its own namespace (`yacht-<16 hex of sha256>`, see
`app.Namespace`). So each app's Ingress must live in the app's namespace, and a
single Secret in one namespace cannot be referenced from fifty others.

Two ways out were considered:

**Replicate the Secret into each app namespace.** Portable across ingress
controllers, and it preserves the config variable as originally conceived. But
it copies the wildcard **private key into every tenant namespace**, where it is
one over-broad RBAC role away from a tenant reading the key that signs every
other tenant's hostname. It also desynchronises on renewal: when the wildcard
rotates, N copies go stale and TLS breaks at expiry, months away from the cause.

**Reference a default certificate configured once in the ingress controller.**
The operator pre-provisions the wildcard as the controller's default
certificate; Yacht creates Ingresses that carry no `secretName`. The private key
stays in exactly one namespace and never enters a tenant's; renewal happens in
one place; there is nothing to keep in sync.

**Decision: the controller default certificate.** The project's security posture
throughout is containment by construction rather than by configuration.
Replication is safe only for as long as nobody grants a tenant `get secret` in
their own namespace — that is a configuration-dependent containment story, and
this codebase has consistently refused those. K3s ships Traefik, which has a
default-certificate concept.

This stays on the correct side of the Traefik position recorded in
`docs/coverage.md`: Yacht *consumes* ingress-controller configuration here, it
does not write it. The operator provisions the default certificate; Yacht never
touches the controller's own config.

### The honest weakness

Yacht cannot verify that the controller actually has a wildcard default
configured. If it does not, apps serve the wrong certificate rather than failing
loudly — the worst failure mode this design has.

It cannot be enforced, so it must at least be stated: a startup log line when
wildcard TLS is enabled, and a visible TLS posture on the settings page. A
capability the engine cannot check is one it should be noisy about.

---

## Components

### New package `internal/domain`

Two responsibilities, deliberately separated.

**Pure policy** — no database, no Kubernetes:

```go
func Issue(appName, appDomain string) (string, error)
func Reserved(host, appDomain string, extra []string) bool
```

**Store access** for `domains` rows: ensure and read the managed row for an app.

This is its own package rather than more of `internal/app` for two reasons.
`internal/app/service.go` is already 526 lines and owns the app lifecycle. And
the reserved-suffix rule is the one piece of this work that is unsafe to get
subtly wrong, so it belongs somewhere pure, directly testable, and reusable by
custom domains later without dragging the app service along.

### `internal/app`

`Create` issues the managed hostname in the same transaction as the app insert.
`apply` reads current hosts, reconciles the managed row against current config,
and passes hosts to the orchestrator.

### `internal/orchestrator`

`AppSpec` gains `Hosts []string` and `TLS bool`. `Validate` rejects malformed
hostnames. No Kubernetes types cross the seam — invariant 1 holds unchanged.

### `internal/orchestrator/k8s`

`applyIngress` writes one Ingress per app, one rule per host, backed by the
existing Service.

It must also **prune**: when hosts are empty or `Port == 0`, an existing Ingress
is deleted rather than left behind. `ApplyApp` today skips the Service when
`Port == 0` (`app.go:44`) but nothing removes a Service that used to exist, so
clearing a port leaves an orphan. That existing gap is fixed here rather than
reproduced for Ingress.

`DeleteApp` deletes the Ingress alongside the Deployment and Service.

**The Ingress object.** Named after the app, in the app's namespace, carrying
the same labels as the Deployment and Service.

- `ingressClassName` is left **unset**, so the cluster's default IngressClass
  applies. Naming a class would hard-code an assumption about which controller
  is installed, which is the coupling this design is otherwise avoiding. An
  install with no default class is an operator misconfiguration that surfaces as
  an unrouted Ingress.
- One rule per host, each with a single path `/` at `pathType: Prefix`. Yacht
  routes a hostname to an app; it does not offer per-path routing, and adding it
  later is additive.
- The backend is the existing Service, referenced by its fixed service port —
  the same stable port the Service already exposes regardless of container port
  (`app.go:24`), which is precisely why the Ingress does not need to know the
  container port.
- When `YACHT_WILDCARD_TLS` is true, a `tls` block listing the hosts and
  **no `secretName`**. That absence is the whole mechanism; a test asserts it,
  because an implementation that "helpfully" fills in a secret name silently
  reintroduces the namespace problem.

### `internal/config`

| Variable | Default | Meaning |
|---|---|---|
| `YACHT_APP_DOMAIN` | empty | Platform domain. Empty means the feature is off. |
| `YACHT_WILDCARD_TLS` | `false` | Emit a TLS section referencing the controller default. |
| `YACHT_RESERVED_DOMAINS` | empty | Comma-separated additional reserved suffixes. |

Startup fails if `YACHT_WILDCARD_TLS` is true while `YACHT_APP_DOMAIN` is empty.
Consistent with the existing preference for unparseable or incoherent settings
failing at startup rather than degrading silently.

`.env.example` must document all three — there is a test that fails if
`config.go` reads a variable the example file does not document.

### `internal/web`

The app URL rendered as a link on the app list and the detail header. Settings
shows the app domain and TLS posture.

---

## Data model

Migration `00002_domains_managed.sql`:

```sql
ALTER TABLE domains ADD COLUMN managed boolean NOT NULL DEFAULT false;

-- An app has at most one platform-issued hostname. Partial, so custom domains
-- stay unconstrained.
CREATE UNIQUE INDEX domains_app_managed_key ON domains (app_id) WHERE managed;
```

Managed rows are created with `verified = true`. A hostname the platform issued
needs no proof of ownership, and leaving it false would mean inventing a
verification step for a name we control.

The existing `domains_host_key` on `lower(host)` is unchanged. It is what
actually prevents two apps claiming one hostname.

Domain queries go in a new `internal/store/queries/domains.sql`; `apps.sql`
stays about apps.

### Why `managed` is a column and not a suffix match

The app domain is configuration and can change. Matching stored hostnames
against the *current* `YACHT_APP_DOMAIN` to decide which rows the platform owns
gives a different answer after that config changes — rows issued under the old
domain would stop being recognised as platform-issued, and a customer domain
could start being recognised as one. A column records what was true at issue
time and does not move.

---

## Issuance and reissue

At create, in the same transaction as the app insert:

- `host = <app-name>.<YACHT_APP_DOMAIN>`
- written as a `domains` row with `managed = true`, `verified = true`

An app can never exist without its URL, and there is no separate "issue domain"
step that could be forgotten or fail independently.

When `YACHT_APP_DOMAIN` is empty, no row is written and no Ingress is created.
The app deploys exactly as it does today.

**On apply**, the managed row is reconciled against current config. If
`YACHT_APP_DOMAIN` has changed, the row is rewritten and the Ingress follows.
So changing the app domain moves each app's URL the next time it is applied —
created, scaled, redeployed. URLs drift over rather than moving at once, and no
startup path mutates every app's routing.

The `managed` column is what makes this safe: the reconcile rewrites only rows
Yacht issued, and can never touch a customer's own domain.

### Hostname collision across owners

`apps` is unique on `(owner_id, name)`; `domains` is unique on `lower(host)`
globally. So two different owners each creating an app named `web` both resolve
to `web.apps.example.com`, and the second insert violates the unique index.

In the engine this cannot happen — it is single-owner by construction. In a
multi-tenant wrapper it can.

The engine's job here is to fail clearly rather than to solve it: translate the
unique violation into a "hostname already taken" error instead of surfacing a
raw constraint error. The multi-tenant answer is a per-tenant app domain
(`*.acme.platform.com`), which is a wrapper concern and is deliberately not
designed into the engine now.

Recorded here so it is not rediscovered later as a surprise.

---

## The reserved-suffix rule

```go
func Reserved(host, appDomain string, extra []string) bool
```

True when `host` equals `appDomain`, is a subdomain of it, or matches any entry
in `YACHT_RESERVED_DOMAINS` the same way.

The rule exists so that the platform suffix is reserved against tenant claims
**automatically**, whether or not an operator remembered to list it in
`YACHT_RESERVED_DOMAINS`. Without it, a tenant could claim
`othertenant.apps.example.com` in the window before the platform issued it.

**The trap this is written to avoid** is string-suffix matching.
`evilapps.example.com` ends with `apps.example.com` as a substring but is not a
subdomain of it. Matching must be on the `.` label boundary. This is the single
most important test in the sub-project.

### Deliberately not yet called

Since custom domains are out of scope, there is no claim path, so `Reserved` has
no caller in this work.

It is built now anyway: it is roughly thirty lines, it is the piece that is
unsafe to retrofit, and writing it beside the hostname policy it mirrors is much
cheaper than bolting it on later. This is the one piece of not-yet-called code in
the sub-project, called out here so it does not read as an oversight.

---

## Error handling

Apply order is Deployment → Service → Ingress. A failure part-way leaves partial
state that the next apply reconciles — the property the existing code already
has, preserved rather than changed.

A unique violation on hostname insert becomes a clear "hostname already taken"
error, not a raw constraint error.

---

## Testing

**Pure (`internal/domain`)**
- `Issue` builds the expected hostname; rejects names that would produce an
  invalid DNS name.
- `Reserved` matches the domain itself and true subdomains.
- `Reserved` does **not** match `evilapps.example.com` against
  `apps.example.com` — the label-boundary trap.
- Case-insensitivity and trailing dots.

**Orchestrator (fake clientset)**
- Ingress created when hosts and port are both set.
- No Ingress when `Port == 0`.
- Ingress pruned when hosts go away or the port is cleared.
- Service pruned when the port is cleared — the pre-existing gap.
- `DeleteApp` removes the Ingress.
- TLS section present or absent per `YACHT_WILDCARD_TLS`, and never carries a
  `secretName`.

**Store (requires Postgres)**
- Migration applies and is idempotent.
- The partial unique index permits many custom rows but one managed row per app.
- `TestEveryTableIsOwnerScoped` still passes with the new column.

**App service**
- Create issues a managed host in the same transaction.
- Changing `YACHT_APP_DOMAIN` reissues on next apply.
- Collision returns the clear error.

**Web**
- App URL rendered and linked.
- `TestEveryNavTargetResolves` still passes.
- A gallery state showing an app with its URL.

**Config**
- `YACHT_WILDCARD_TLS` without `YACHT_APP_DOMAIN` fails at startup.
- `.env.example` documents all three new variables.

---

## Relationship to other work

Three sub-projects were separated out of the lost commits:

- **A — this document.** Per-app hostnames and shared wildcard TLS. Independent.
- **B — accounts, teams, email.** `internal/account`, `internal/notify`, the
  `owners` → `teams` rename.
- **C — sign-in, role gates, team management.** The HTTP surface over B; not
  meaningfully separable from it.

A is first because it fixes the broken core loop and depends on neither of the
others.

**Volume resize is not part of A.** It was originally described as a small
addition, but this repository has no volume *provisioning* at all: `AppSpec` has
no volume field, nothing creates a PVC, and `/cluster/volumes` is a read-only
cluster-wide listing. Resize therefore needs the whole storage subsystem first —
a per-app storage tab, PVC creation, attachment to the Deployment — which is its
own sub-project of comparable size to A.

## Deferred

Customer custom domains: claim → prove by DNS TXT → publish, per-host
cert-manager Certificates, and per-host certificate status and expiry in the UI.
The `domains` table already carries `verified` for this, and `Reserved` is built
ready for it.
