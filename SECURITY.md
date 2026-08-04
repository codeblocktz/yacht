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
- **No account lockout after repeated failed passwords.** Attempts are
  throttled — five per address and twenty per client every fifteen minutes —
  but an address is never locked. A lockout on a known address is a denial of
  service against the person who owns it, and the emailed link stays available
  regardless, so the password is never the only way in to attack.
- **Password attempts and sign-in-link requests are counted separately.** That
  is deliberate: a shared counter would let somebody exhaust a victim's link
  budget by guessing at their password.
- **Rate limits are per process and held in memory.** A restart forgives
  everyone, and two replicas each keep their own count. They are a brake on
  guessing, not a boundary.
- **No check against breached-password lists.** An offline list is too large to
  ship with a self-hosted binary, and an online one makes setting a password
  depend on an outbound call from a box that may not have one.
- **A magic link is a full account-recovery path.** Anybody holding the mailbox
  can sign in and change or remove the password. That is what the link is, and
  it is why there is no separate reset token.
- Anything that needs an attacker who already has root on the host or
  cluster-admin on the cluster.

## A standing caveat

Yacht has not been audited, and has not run anywhere long enough to have earned
your trust. It is not ready for production. Treat an install as you would any
young piece of infrastructure holding credentials.
