# Security policy

## Reporting a vulnerability

**Please do not open a public issue.**

Report privately through GitHub:
[**Report a vulnerability**](https://github.com/codeblocktz/yacht/security/advisories/new).
That opens a draft advisory only you and the maintainers can see.

If you cannot use GitHub, email the maintainer address on the commits in this
repository, with `SECURITY` in the subject.

What to expect:

| | |
|---|---|
| First reply | Within 7 days |
| Assessment | Within 14 days of the first reply |
| Fix and advisory | As fast as the severity warrants |

This is a small project maintained by one person, so those are targets rather
than guarantees. If a week passes with no reply, assume the message was missed
and send it again.

You will be credited in the advisory unless you ask not to be. There is no
bounty programme.

## Supported versions

**None yet.** There has been no tagged release, so there is no released version
to backport a fix to. Security fixes land on `main`, and the only supported
thing is the current commit.

When releases begin, this section will say which ones get fixes.

## Scope

In scope — anything that lets somebody:

- read or change another team's apps, logs, secrets, or database rows
- escape the security context Yacht applies to workloads (see
  [Security posture](README.md#security-posture))
- reach the cluster's credentials, the join token, or `YACHT_SECRET_KEY`
- bypass authentication or session handling

Out of scope, because they are documented behaviour rather than undiscovered
weaknesses:

- **The installer serves the dashboard over plain HTTP.** The bearer token
  crosses the network in the clear until an operator puts it behind TLS. The
  README and the installer's own output both say so.
- **No `YACHT_AUTH_TOKEN` and no accounts means an unauthenticated dashboard.**
  That is what the configuration table says that combination does.
- Anything that needs an attacker who already has root on the host or
  cluster-admin on the cluster.

## A standing caveat

Yacht has not been audited, and has not run anywhere long enough to have earned
your trust. It is not ready for production. Treat an install as you would any
young piece of infrastructure holding credentials.
