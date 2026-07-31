# Accounts, teams, email, and sign-in

**Date:** 2026-07-31
**Status:** approved, not yet implemented
**Scope:** sub-projects B and C of four (see "Relationship to other work")

---

## The problem

Yacht has authorization but no authentication. `identity.Provider` answers "who
owns this request?" and `identity.Middleware` gates every route group — that
part is solid. But the only two implementations are `SingleOwner`, which
authenticates nothing, and `StaticToken`, one shared bearer token.

There is no way to add a colleague. No users, no teams, no roles, no sessions,
no sign-in page. Grepping for `login|session|cookie|password|magic` across all
Go and templ sources returns five hits, all comments — two of which refer to a
"future login screen" that was never built.

This makes Yacht a single-person tool. Everything about collaboration is
downstream of fixing it.

---

## What this delivers

**B — the foundation.** Two packages and a schema move:

- `internal/account` — users, teams, memberships, sessions, invitations
- `internal/notify` — the fourth seam: one `Mailer` interface with SMTP and
  Resend behind it, plus a logging fallback so an install works with neither
  configured
- `owners` renamed to `teams`

**C — the surface.** Magic-link sign-in, sessions, sign-out everywhere,
invitation accept flow, a team switcher, and a team management page (invite,
revoke, change role, remove). Roles enforced on route groups.

---

## How this survives the identity seam

`internal/identity` currently states that the engine is single-owner and
deliberately does not own authentication, on the grounds that baking in session
handling would force a wrapping application to fight it.

This work makes the first half of that untrue: an install will host several
teams. The comment must be **rewritten as part of this change**, not left to
quietly contradict the code.

The second half survives, and it is the part that matters. `account` provides a
third `identity.Provider` implementation rather than replacing the seam:

| Provider | Resolves an owner from |
|---|---|
| `SingleOwner` | configuration — authenticates nothing |
| `StaticToken` | a shared bearer token |
| **`account.Sessions`** | **a session cookie, scoped to the active team** |

`identity.Middleware`, every handler, the app service, the store and the
orchestrator are all untouched, because they already take an `OwnerID` and never
ask where it came from. A wrapping application still swaps the whole provider
out.

**A team IS an owner.** That is what makes this cheap: `owner_id` continues to
mean "the team that owns this row", so apps, deployments and domains need no
query changes at all.

---

## Data model

### The rename

```sql
ALTER TABLE owners RENAME TO teams;
```

Postgres carries foreign keys, indexes and constraints through a rename, so
every existing `owner_id` reference follows automatically. No data migration, no
query rewrite, no audit of existing call sites.

This is the return on the original decision to put `owner_id` on every table and
scope unique constraints by it. Had ownership been a join through two other
tables, this would be a rewrite instead of one statement.

Column names stay `owner_id`. Renaming them to `team_id` would touch every query
and generated struct for no behavioural gain.

### New tables

- **`users`** — `id`, `email` (citext or lower-unique), `display_name`,
  `totp_secret`, `totp_confirmed`, timestamps. A person, independent of any
  team.
- **`memberships`** — `user_id`, `team_id`, `role`, `created_at`. Unique on
  `(user_id, team_id)`. This is where roles live.
- **`sessions`** — `id`, `user_id`, `token_hash`, `active_team_id`,
  `expires_at`, `created_at`, plus user agent and IP for a sessions list.
- **`invitations`** — `id`, `team_id`, `email`, `role`, `token_hash`,
  `invited_by`, `expires_at`, `accepted_at`.

TOTP columns are carried now and left unused. Adding a column later is a
migration across live data; carrying two nullable ones costs nothing and means
2FA does not need a schema change.

### The owner-scoping invariant needs an honest exception

`TestEveryTableIsOwnerScoped` currently exempts only `owners` and
`goose_db_version`. `users` and `sessions` genuinely are not owner-scoped — a
user exists before belonging to any team, and a session belongs to a person, not
a team.

The exemption list must be extended **with a stated reason per entry**, not
quietly loosened. `memberships` and `invitations` are team-scoped and stay under
the invariant. If that test is weakened without comment, the next table to skip
`owner_id` will do so silently, and that is the failure the test exists to catch.

---

## Roles

Owner / Admin / Member. The split is at the line where **an action stops being
undoable by redeploying**:

| | Member | Admin | Owner |
|---|---|---|---|
| Deploy, scale, redeploy | ✅ | ✅ | ✅ |
| Delete apps | | ✅ | ✅ |
| Manage domains and storage | | ✅ | ✅ |
| Invite and remove members | | ✅ | ✅ |
| Manage admins, transfer ownership, delete team | | | ✅ |

Enforcement is `r.Use(s.requireRole(...))` on the route **group**, never a check
inside a handler. A check every handler has to remember is a check some handler
will eventually forget, and the forgotten one is the security bug. This is the
same reasoning `identity.Middleware` already uses, applied one level further in.

A team must always have at least one Owner. Removing or demoting the last one is
refused.

---

## Sign-in

Magic link only. No passwords to store, leak, or force anyone to rotate.

1. Visitor submits an email address.
2. If an account exists, a signed link is mailed. **If not, nothing is mailed —
   and the response is identical.**
3. The link carries a single-use token, valid for a short window.
4. Following it creates a session and sets a cookie.

### What must hold

- **Tokens are stored as SHA-256 hashes, never as tokens.** A database dump must
  not be a set of working credentials — for sessions, magic links and
  invitations alike.
- **No account enumeration.** Same response, same status, same timing, same rate
  limiting whether or not the address exists. A sign-in form that reveals which
  addresses are registered is a user-list disclosure.
- **Single use, short TTL.** A magic link in a mail archive is a permanent key
  otherwise.
- Cookies `HttpOnly` and `SameSite=Lax`. That is the CSRF defence, which is why
  no form carries a token. `Secure` only when the request arrived over TLS —
  setting it unconditionally makes sign-in silently impossible on a plain-HTTP
  install, with nothing on screen to explain why.
- Sign-out everywhere deletes every session for the user, not just the current
  one. Without it a leaked session cannot be revoked.

---

## The lockout problem

This is the failure mode most likely to hurt, and it deserves its own section.

Turning sign-in on with no way to deliver a link locks an operator out of their
own dashboard, permanently, with no recovery path. The install still runs; they
simply cannot get in.

Three things guard against it:

1. **Accounts activate only when a mail transport or `YACHT_BASE_URL` is
   configured.** With neither, the engine stays on `SingleOwner` or
   `StaticToken` and says so at startup.
2. **The first person to sign in inherits `YACHT_OWNER_ID`.** An existing
   single-owner install keeps the apps already deployed there, rather than
   stranding them under an owner nobody can authenticate as.
3. **The logging Mailer is the break-glass path.** With no SMTP or Resend
   configured, magic links are written to the log. This must be **deliberate and
   documented**, not incidental: it is the only way back in when mail delivery
   breaks after accounts are switched on. It is also, obviously, only safe
   because reading the log already implies host access.

---

## The notification seam

`internal/notify` is seam 4. One interface:

```go
type Mailer interface {
    Send(ctx context.Context, msg Message) error
}
```

Three implementations ship: `SMTP`, `Resend`, and `Log`. A wrapping application
supplies Slack, Discord, or a queue without touching the engine.

`Log` is the zero-configuration default rather than an error, so an install
works out of the box — see the lockout section for why that specific fallback
matters.

The seam count in every package doc comment (`SEAM n of 3`) becomes `of 4`.

---

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `YACHT_BASE_URL` | empty | Public URL, used to build magic links. Enables accounts. |
| `YACHT_SMTP_ADDR` | empty | SMTP host:port. Enables the SMTP mailer. |
| `YACHT_SMTP_USER` / `YACHT_SMTP_PASSWORD` | empty | SMTP credentials. |
| `YACHT_SMTP_FROM` | empty | Envelope sender. Required with SMTP. |
| `YACHT_RESEND_API_KEY` | empty | Enables the Resend mailer. |
| `YACHT_SESSION_TTL` | `720h` | Session lifetime. |
| `YACHT_MAGIC_LINK_TTL` | `15m` | Magic-link lifetime. |

All must appear in `.env.example` — `TestEveryVariableIsDocumented` now scans
`config.go` for every `YACHT_*` string it reads, so an undocumented one fails
the build.

Incoherent combinations fail at startup: SMTP configured without a from address,
Resend and SMTP both configured, a `YACHT_BASE_URL` that is not a valid URL.

---

## Testing

**Roles** — a table-driven test asserting every route group's required role, so
adding a route without a gate is visible. Plus: the last Owner cannot be removed
or demoted.

**Sign-in** — no enumeration (identical response and status for known and
unknown addresses); magic-link token is single-use; an expired token is
rejected; tokens are never stored in plaintext (assert the column contains a
hash, not the token).

**Sessions** — sign-out everywhere invalidates all sessions; an expired session
resolves to unauthenticated; the cookie carries `HttpOnly` and `SameSite=Lax`,
and `Secure` only over TLS.

**Isolation** — the one that matters most: a member of team A cannot read, list,
or mutate an app of team B, exercised through the HTTP layer rather than the
store. `TestTenantCannotClaimUnderTheAppDomain`-style naming is fine; the
`make boundary` check excludes `_test.go`.

**Migration** — the rename preserves every foreign key and all existing rows;
`TestEveryTableIsOwnerScoped` passes with the extended exemption list.

**Mailer** — `Log` is used when nothing is configured; SMTP and Resend are
selected when configured; a send failure does not lose the sign-in attempt
silently.

---

## Relationship to other work

- **A — done.** Per-app hostnames and shared wildcard TLS (`67fa665..cb9989c`).
  Not yet proven against a real cluster.
- **B and C — this document.** Split into two plans: B is schema and packages,
  C is the HTTP surface. A single plan would exceed the workflow size guideline,
  and a checkpoint between them is worth having because B carries the schema
  risk.
- **Volumes — next.** Provisioning, attachment, then resize. Independent of this.

## Deferred

TOTP enrolment (columns are carried, no flow), project-level permissions, Slack
/ Discord / Telegram channels, and alert rules — which are also the missing event
source those channels would fire from.
