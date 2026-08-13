# Contributing

Issues and pull requests are welcome. There is no CLA — contributions are MIT
licensed, the same as the project.

Yacht is early and moving. If you are about to spend real time on something,
open an issue first and check the direction is one the project wants, so the
work is not wasted.

## Getting set up

You need **Go 1.26+**, **Postgres**, and a **kubeconfig** pointing at a cluster.
Running the full verification also needs **shellcheck**. `templ` and `sqlc` are
Go tool dependencies, and `make css` downloads the Tailwind standalone binary,
so there is no Node or npm.

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
> looks like a pass.** `make check` therefore refuses to start without a test
> database unless you explicitly request an intentionally partial run.

No Postgres to hand? `make check-db` supplies a throwaway one and tears it down
afterwards:

```bash
make check-db       # vet + tests, on a database that exists for the run
make verify-db      # the full gate, same idea
```

That uses [popgres](https://github.com/algolab-cloud/popgres), which downloads
real Postgres binaries once per version and runs them as an ordinary local
process on a free port — no Docker, and nothing installed system-wide. It is
reached through `npx`, so it needs Node but no install step; set `POPGRES` to a
native binary if you would rather not use npm. An instance you started yourself
with `popgres up` is reused and left running.

Otherwise point the DSN at a database you do not mind being written to:

```bash
export YACHT_TEST_DATABASE_URL="postgres://yacht:yacht@localhost:5432/yacht_test?sslmode=disable"
make check          # vet + tests
```

If a database is deliberately unavailable, use
`YACHT_ALLOW_DATABASE_TEST_SKIPS=1 make check`. That opt-out is visible in the
command and should not be used for release verification.

## The commands

| | |
|---|---|
| `make assets` | templ codegen + Tailwind — run after touching a `.templ` or the CSS |
| `make check` | `go vet` + database-backed race tests; requires the test DSN |
| `make check-db` | `make check` on a throwaway Postgres, torn down afterwards |
| `make verify` | Every CI/release gate, including generated output and gallery rendering |
| `make verify-db` | `make verify` on a throwaway Postgres |
| `make dev` | Rebuild and run |
| `make gallery` | Render every visual state to HTML |
| `make sqlc` | Regenerate database code after editing a `.sql` query |

## What CI enforces

`make verify` is the shared CI and release contract. Its gates are runnable
locally and exist because each catches a class of bug that otherwise ships:

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
5. **The complete gallery renders.** CI keeps the generated HTML as a reviewable
   artifact so visual states do not silently rot.
6. **Every Go package compiles without CGO.** Release builds add the supported
   Linux architecture builds after this shared verification succeeds.

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
- **Run `make verify` before pushing.**

Small, obviously-correct fixes — typos, a broken link, a clearer error message —
need none of the ceremony. Just send them.

## Reporting things

- **Bugs and features:** open an issue; the forms ask for what is usually needed.
- **Vulnerabilities:** do not open an issue. See [SECURITY.md](SECURITY.md).
- **Conduct:** see [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
