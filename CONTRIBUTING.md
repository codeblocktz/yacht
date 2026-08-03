# Contributing

Issues and pull requests are welcome. There is no CLA — contributions are MIT
licensed, the same as the project.

Yacht is early and moving. If you are about to spend real time on something,
open an issue first and check the direction is one the project wants, so the
work is not wasted.

## Getting set up

You need **Go 1.26+**, **Postgres**, and a **kubeconfig** pointing at a cluster.
Nothing else — `templ` and `sqlc` are Go tool dependencies, and `make css`
downloads the Tailwind standalone binary, so there is no Node or npm.

```bash
git clone https://github.com/codeblocktz/yacht.git
cd yacht

export YACHT_DATABASE_URL="postgres://yacht:yacht@localhost:5432/yacht?sslmode=disable"
export YACHT_KUBECONFIG="$HOME/.kube/config"
export YACHT_AUTH_TOKEN="$(openssl rand -hex 24)"

make dev
```

No cluster to hand? Yacht still boots and says so on the overview page, so most
of the dashboard is workable without one.

### Run the tests properly

> [!IMPORTANT]
> **Tests that need a database skip themselves when it is absent, and a skip
> looks like a pass.** `make check` can print `ok` for every package while the
> tests that check owner scoping never ran.

Set the DSN to a database you do not mind being written to:

```bash
export YACHT_TEST_DATABASE_URL="postgres://yacht:yacht@localhost:5432/yacht_test?sslmode=disable"
make check          # vet + tests
```

Check the output for `SKIP` before believing a green run.

## The commands

| | |
|---|---|
| `make assets` | templ codegen + Tailwind — run after touching a `.templ` or the CSS |
| `make check` | `go vet` + tests |
| `make dev` | Rebuild and run |
| `make gallery` | Render every visual state to HTML |
| `make sqlc` | Regenerate database code after editing a `.sql` query |

## What CI enforces

Four things fail a build. All four are runnable locally, and all four exist
because the alternative is a bug nobody sees:

1. **Generated output is current.** `*_templ.go` and `internal/web/assets/css/app.css`
   are committed so plain `go build` works without codegen tools. CI regenerates
   both and fails on drift. Run `make assets` and commit the result.
2. **`go vet` and the tests pass**, with `-race`.
3. **No commercial concepts in the engine.** A grep rejects a `type`, `func`,
   `var`, or `const` declaring a tenant, wallet, billing, invoice, or
   subscription. Yacht must stay useful standalone at a single owner; those
   concepts belong to a wrapping layer. Vendored UI is excluded — a Lucide icon
   named `Wallet` is a picture, not a billing concept.
4. **`shellcheck` on `install.sh` and `upgrade.sh`.** They get piped into a root
   shell on somebody else's machine.

## Things this codebase cares about

Read a few files before writing new ones — the conventions are visible and
fairly consistent.

**Comments say why, not what.** The code already shows what it does. A comment
earns its place by explaining the reason a choice was made, or the bug the
choice avoids. `internal/app/logs.go` and `internal/orchestrator/orchestrator.go`
are representative.

**Three seams stay clean.** Orchestration, identity, and dashboard chrome are
interfaces so a larger application can build on this module rather than fork it.
Two rules hold them:

- No Kubernetes type crosses `orchestrator`. Callers never import `client-go`.
- Every table carries `owner_id`, and unique constraints are scoped by it.

**Scoping is a predicate, not a convention.** Every query filters by `owner_id`
in the SQL, so a handler that forgets to scope returns nothing rather than
somebody else's data. If you add a query, scope it there.

**Visual states go in the gallery.** `internal/web/gallery_test.go` renders every
state the dashboard can be in — a degraded workload, a failed deploy, an empty
month — without a browser or a cluster. Those are the states that rot unseen. If
you add a state worth looking at, add it there and it stays checked.

## Pull requests

- **One change per PR.** A refactor and a fix in one branch is two reviews
  wearing a trenchcoat.
- **Write the commit message for the reader.** Imperative subject in the present
  tense, and a body explaining why the change is right — not a restatement of
  the diff. `git log` is the house style guide.
- **Tests come with behaviour changes.** For a bug fix, a test that fails before
  the fix and passes after.
- **Run `make check` and `make assets` before pushing.**

Small, obviously-correct fixes — typos, a broken link, a clearer error message —
need none of the ceremony. Just send them.

## Reporting things

- **Bugs and features:** open an issue; the forms ask for what is usually needed.
- **Vulnerabilities:** do not open an issue. See [SECURITY.md](SECURITY.md).
- **Conduct:** see [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
