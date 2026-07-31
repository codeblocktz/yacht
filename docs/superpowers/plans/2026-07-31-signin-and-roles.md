# Sign-in, Role Gates and Team Management (Sub-project C) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Put a sign-in surface on the accounts foundation, so an operator can actually add a colleague — magic-link sign-in, sessions in a cookie, roles enforced on route groups, a team switcher and a team management page.

**Architecture:** Everything sits on `internal/account`, which already exists and is tested. This plan adds HTTP only: handlers, templates, cookie handling, and `requireRole` middleware mounted on route *groups*. No new schema except one migration for a defect carried over from B.

**Tech Stack:** Go 1.26, chi, templ, Tailwind CLI, `internal/account`, `internal/notify`.

**Spec:** `docs/superpowers/specs/2026-07-31-accounts-teams-email-design.md`

## Global Constraints

- **Use Go 1.26.** Bare `go` may be a ServBay shim at 1.20.5. Check `go version`; if wrong, `export PATH=/usr/local/go/bin:$PATH`.
- **Tests need Postgres:** `export YACHT_TEST_DATABASE_URL="postgres://postgres:postgrespw@localhost:5432/yacht_test?sslmode=disable"`.
- **Never write a credential into a committed file.** The DSN above is for your shell only — do not put the password in `.env.example`, docs, or test fixtures.
- Roles are enforced with `r.Use(...)` on route **groups**, never inside handlers. A check a handler must remember is one some handler will forget.
- Cookies: `HttpOnly` and `SameSite=Lax` always. `Secure` **only** when the request arrived over TLS — setting it unconditionally makes sign-in silently impossible on a plain-HTTP install.
- `SameSite=Lax` **is** the CSRF defence. Do not add CSRF tokens to forms; do ensure every mutating route is POST.
- templ output and the stylesheet are committed. Run `make generate` and `make css`; CI fails on drift.
- `.env.example` must document every `YACHT_*` variable. `TestEveryVariableReadIsDocumented` scans for them.
- `go vet ./...` and `go test ./... -race -count=1` clean before every commit.
- Comments explain **why**, not what.

---

## File Structure

**Create:**
- `internal/web/auth.go` — cookie helpers, sign-in/sign-out/callback handlers
- `internal/web/auth_test.go`
- `internal/web/roles.go` — `requireRole` middleware
- `internal/web/roles_test.go`
- `internal/web/team.go` — team switcher, member management, invitations
- `internal/web/team_test.go`
- `internal/web/auth.templ` — sign-in page, check-your-mail page, invitation accept
- `internal/web/team.templ` — team management page
- `internal/store/migrations/00005_invitation_cleanup.sql`

**Modify:**
- `internal/web/server.go` — route groups, `Options`, session plumbing
- `internal/web/slots.go` — team switcher in the sidebar
- `internal/web/layout.templ` — signed-in chrome
- `internal/account/account.go` — revoke a removed member's invitations
- `internal/account/magiclink.go` — first-user bootstrap
- `cmd/yacht/main.go` — real cookie name, wire the account service
- `.env.example`

---

### Task 1: Cookie handling and the session cookie name

**Files:** Create `internal/web/auth.go`, `internal/web/auth_test.go`. Modify `cmd/yacht/main.go`.

**Interfaces:**
- Produces: `const SessionCookie = "yacht_session"`, `func setSessionCookie(w http.ResponseWriter, r *http.Request, raw string, ttl time.Duration)`, `func clearSessionCookie(w http.ResponseWriter, r *http.Request)`, `func requestIsTLS(r *http.Request) bool`

- [ ] **Step 1: Write the failing test**

```go
package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSessionCookieFlags(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://yacht.test/", nil)

	setSessionCookie(w, r, "token-value", time.Hour)

	cs := w.Result().Cookies()
	if len(cs) != 1 {
		t.Fatalf("cookies = %d, want 1", len(cs))
	}
	c := cs[0]

	if c.Name != SessionCookie || c.Value != "token-value" {
		t.Fatalf("cookie = %s=%s", c.Name, c.Value)
	}
	if !c.HttpOnly {
		t.Error("HttpOnly must be set — script-readable session cookies are stealable by XSS")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Error("SameSite=Lax is the CSRF defence; without it every form needs a token")
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want /", c.Path)
	}
	// Secure on a plain-HTTP request means the browser silently drops the
	// cookie and sign-in appears to do nothing, with nothing to explain why.
	if c.Secure {
		t.Error("Secure must not be set on a plain-HTTP request")
	}
}

func TestSessionCookieIsSecureOverTLS(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "https://yacht.test/", nil)
	r.TLS = &tls.ConnectionState{}

	setSessionCookie(w, r, "token-value", time.Hour)

	if !w.Result().Cookies()[0].Secure {
		t.Error("Secure must be set when the request arrived over TLS")
	}
}

// A reverse proxy terminates TLS and forwards plain HTTP. Without honouring
// the header, every proxied install loses the Secure flag.
func TestSessionCookieHonoursForwardedProto(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://yacht.test/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")

	setSessionCookie(w, r, "token-value", time.Hour)

	if !w.Result().Cookies()[0].Secure {
		t.Error("Secure must be set when X-Forwarded-Proto says https")
	}
}

func TestClearSessionCookieExpiresIt(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://yacht.test/", nil)

	clearSessionCookie(w, r)

	c := w.Result().Cookies()[0]
	if c.Value != "" || c.MaxAge >= 0 {
		t.Fatalf("cleared cookie = %q MaxAge=%d, want empty with negative MaxAge", c.Value, c.MaxAge)
	}
}
```

Add `"crypto/tls"` to the imports.

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/web/ -run Cookie -v`. Expected: `undefined: setSessionCookie`.

- [ ] **Step 3: Implement**

```go
// SessionCookie is the name of the cookie carrying a session token.
const SessionCookie = "yacht_session"

// requestIsTLS reports whether the request reached us over TLS.
//
// X-Forwarded-Proto is honoured because the common deployment terminates TLS
// at a reverse proxy and forwards plain HTTP. Reading only r.TLS would drop
// the Secure flag on exactly the installs that have a certificate.
func requestIsTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// setSessionCookie writes the session cookie.
//
// Secure is conditional on purpose. Setting it unconditionally makes the
// browser drop the cookie on a plain-HTTP install, so sign-in appears to
// succeed and then does nothing, with nothing on screen explaining why.
//
// SameSite=Lax is the CSRF defence, which is why no form in this codebase
// carries a token: a cross-site POST does not get the cookie attached.
func setSessionCookie(w http.ResponseWriter, r *http.Request, raw string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsTLS(r),
		MaxAge:   int(ttl.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: requestIsTLS(r), MaxAge: -1,
	})
}
```

In `cmd/yacht/main.go`, replace `Provider("")` with `Provider(web.SessionCookie)`. An empty cookie name resolves nothing — B shipped it as a placeholder alongside a warning.

- [ ] **Step 4: Run tests, 5: Commit**

```bash
git commit -m "Add session cookie handling

Secure is conditional on the request actually being TLS, including behind
a proxy that sets X-Forwarded-Proto. Setting it unconditionally makes the
browser drop the cookie on a plain-HTTP install, so sign-in appears to
work and then does nothing.

SameSite=Lax is the CSRF defence, which is why no form here carries a
token."
```

---

### Task 2: Sign-in page and magic-link request

**Files:** Create `internal/web/auth.templ` (sign-in + check-your-mail). Modify `internal/web/auth.go`, `internal/web/server.go`.

**Interfaces:**
- Consumes: `account.Service.IssueMagicLink`, `notify.Mailer`.
- Produces: `GET /sign-in`, `POST /sign-in`, both **outside** the authenticated group.

- [ ] **Step 1: Write the failing test**

Tests that must exist and pass:

```go
// The whole point of the page.
func TestSignInPageRendersOutsideAuth(t *testing.T)

// A sign-in form that reveals which addresses are registered is a user-list
// disclosure. Status, body and headers must be byte-identical.
func TestSignInDoesNotRevealWhetherAnAddressExists(t *testing.T)

// The spec requires "same timing". A registered address does an extra INSERT,
// so without a floor the difference is measurable — B's adversarial review
// measured 3.5x.
func TestSignInResponseTimeDoesNotLeakExistence(t *testing.T)

// A link that never arrives must not look like success forever.
func TestSignInWithNoMailTransportStillSucceeds(t *testing.T)

func TestSignInRejectsAMalformedAddress(t *testing.T)
func TestSignInIsRateLimited(t *testing.T)
```

`TestSignInDoesNotRevealWhetherAnAddressExists` must compare the full response body and status for a registered and an unregistered address and fail on any difference.

`TestSignInResponseTimeDoesNotLeakExistence` must interleave at least 100 samples of each and assert the ratio of medians is below 1.5.

- [ ] **Step 2: Run to verify they fail. Step 3: Implement.**

The handler:

1. Parse and validate the address; on malformed input render the form with an error.
2. Call `IssueMagicLink`. It returns `existed`.
3. If `existed`, send the mail through the `Mailer`. If not, send nothing.
4. **Always render the same "check your mail" page**, regardless.

**The timing floor.** Wrap the handler body so every response takes at least a fixed duration:

```go
// signInFloor is the minimum time a sign-in POST takes.
//
// Issuing a link for a registered address does an extra INSERT that an
// unregistered address does not, which is measurable — a 3.5x difference was
// demonstrated against this code. Padding to a floor removes the signal
// without pretending the two paths do equal work.
//
// The floor is above the slow path, so the pad is always a wait rather than a
// race the fast path can lose.
const signInFloor = 250 * time.Millisecond

func withFloor(d time.Duration, fn func()) {
	start := time.Now()
	fn()
	if rest := d - time.Since(start); rest > 0 {
		time.Sleep(rest)
	}
}
```

Mail sending must happen **inside** the floor, and a send failure must be logged but must not change the response — otherwise delivery failure becomes an oracle too.

Rate limit by address and by client IP. A floor makes each attempt cost 250ms, so the limit is what stops parallel enumeration.

- [ ] **Step 4: Run tests, 5: `make generate && make css`, 6: Commit**

---

### Task 3: Magic-link callback, session creation, and first-user bootstrap

**Files:** Modify `internal/web/auth.go`, `internal/account/magiclink.go`, `cmd/yacht/main.go`.

**Interfaces:**
- Produces: `GET /auth/{token}`, and `func (s *Service) BootstrapOwner(ctx context.Context, teamID, teamName string, user User) error`

- [ ] **Step 1: Write the failing test**

```go
// The spec's third lockout guard, dropped between spec and plan in
// sub-project B. Without it an existing single-owner install switches
// accounts on and the apps already deployed under YACHT_OWNER_ID belong to a
// team nobody can sign in to.
func TestFirstSignInInheritsTheConfiguredOwner(t *testing.T)

// ...and only the first. The second person must not be handed ownership of
// the existing team just for signing in.
func TestSecondSignInDoesNotInheritOwnership(t *testing.T)

func TestValidTokenCreatesASessionAndRedirects(t *testing.T)
func TestConsumedTokenIsRejected(t *testing.T)
func TestExpiredTokenIsRejected(t *testing.T)
// The cookie must be set on the callback, with the flags from Task 1.
func TestCallbackSetsTheSessionCookie(t *testing.T)
```

`TestFirstSignInInheritsTheConfiguredOwner`: with `YACHT_OWNER_ID=owner-local` and an app already created under it, the first person to complete sign-in must become **Owner of the team `owner-local`** and see that app — not a fresh empty team.

- [ ] **Step 2: fail, Step 3: implement**

`BootstrapOwner` must be idempotent and race-safe: it runs inside a transaction, and claims the configured team **only if that team currently has no owner at all**. Two people completing sign-in simultaneously must produce exactly one owner. Use `SELECT ... FOR UPDATE` on the team row, or an insert guarded by `WHERE NOT EXISTS (SELECT 1 FROM memberships WHERE owner_id = $1)`.

If the configured team already has an owner, the new user gets their own team instead.

- [ ] **Step 4: run, 5: Commit**

```bash
git commit -m "Create a session from a magic link, and hand the first person the existing team

The spec's third lockout guard, which went missing between spec and plan.
An install that switches accounts on has apps already deployed under
YACHT_OWNER_ID; without this they belong to a team nobody can sign in to.

Only the first person, and only when that team has no owner at all —
checked inside the transaction so two simultaneous sign-ins cannot both
claim it."
```

---

### Task 4: Sign-out, and sign-out everywhere

**Files:** Modify `internal/web/auth.go`, `internal/web/server.go`.

**Interfaces:** `POST /sign-out`, `POST /sign-out-everywhere`.

- [ ] **Step 1: Write the failing test**

```go
func TestSignOutClearsTheCookieAndRevokesTheSession(t *testing.T)
// Without this a leaked session cannot be revoked at all.
func TestSignOutEverywhereInvalidatesEveryDevice(t *testing.T)
// GET must not sign anyone out: a prefetched or crawled link would.
func TestSignOutRejectsGET(t *testing.T)
```

- [ ] **Steps 2-4: fail, implement, verify.** Both routes POST only. `POST /sign-out` calls `RevokeSession` then clears the cookie; `/sign-out-everywhere` calls `RevokeAllSessions`.

- [ ] **Step 5: Commit**

---

### Task 5: Role gates on route groups

**Files:** Create `internal/web/roles.go`, `internal/web/roles_test.go`. Modify `internal/web/server.go`.

**Interfaces:** `func (s *Server) requireRole(min account.Role) func(http.Handler) http.Handler`

- [ ] **Step 1: Write the failing test**

The important one is table-driven over the **actual route table**:

```go
// TestEveryMutatingRouteHasARoleGate walks the router and fails if a POST
// route is reachable without a role requirement. A route added next year
// inherits the gate only if the gate is on the group — this is what proves it
// still is.
func TestEveryMutatingRouteHasARoleGate(t *testing.T)

func TestMemberCanDeployButNotDelete(t *testing.T)
func TestAdminCanDeleteAppsAndInvite(t *testing.T)
func TestOnlyOwnerCanManageAdminsOrDeleteTheTeam(t *testing.T)
// The gate must read the role from the session, never from a form field or
// header the caller controls.
func TestRoleCannotBeAssertedByTheClient(t *testing.T)
```

`TestEveryMutatingRouteHasARoleGate` uses `chi.Walk` to enumerate routes and asserts each POST route is inside a gated group. Allowlist only `/sign-in`, `/sign-out`, `/sign-out-everywhere` and the invitation accept route, each with a stated reason.

- [ ] **Steps 2-4.** `requireRole` reads `account.Session.Role`, which resolution already provides — the session query joins `memberships`, so the role arrives proven rather than re-queried.

Group layout in `server.go`:

```go
r.Group(func(r chi.Router) {
    r.Use(identity.Middleware(s.ident))

    // Member: everything that a redeploy can undo.
    r.Group(func(r chi.Router) {
        r.Use(s.requireRole(account.RoleMember))
        r.Get("/", s.overview)
        r.Post("/apps", s.appCreate)
        r.Post("/apps/{name}/scale", s.appScale)
        r.Post("/apps/{name}/redeploy", s.appRedeploy)
    })

    // Admin: everything that a redeploy cannot.
    r.Group(func(r chi.Router) {
        r.Use(s.requireRole(account.RoleAdmin))
        r.Post("/apps/{name}/delete", s.appDelete)
        r.Post("/team/invite", s.teamInvite)
        r.Post("/team/invitations/{id}/revoke", s.teamRevokeInvite)
        r.Post("/team/members/{id}/remove", s.teamRemoveMember)
    })

    // Owner: everything that changes who can administer.
    r.Group(func(r chi.Router) {
        r.Use(s.requireRole(account.RoleOwner))
        r.Post("/team/members/{id}/role", s.teamSetRole)
    })
})
```

- [ ] **Step 5: Commit**

```bash
git commit -m "Gate routes by role on the group, not in the handler

The split is at the line where an action stops being undoable by
redeploying. A check each handler has to remember is one some handler will
forget, and the forgotten one is the security bug — so the gate is on the
group and a route added later inherits it whether or not its author
thought about it.

The role comes from the session, which the resolving query already proved
by joining memberships. It is never read from anything the caller sends."
```

---

### Task 6: Team switcher

**Files:** Create/modify `internal/web/team.go`, `internal/web/slots.go`, `internal/web/layout.templ`.

**Interfaces:** `POST /teams/switch`

- [ ] **Step 1: Write the failing test**

```go
// The one that matters: switching is an authorization decision, not a
// preference. A crafted POST must not move a session into someone else's team.
func TestCannotSwitchIntoATeamYouAreNotIn(t *testing.T)
func TestSwitchingChangesWhichAppsAreVisible(t *testing.T)
func TestSwitcherListsOnlyYourTeams(t *testing.T)
func TestSwitchRejectsGET(t *testing.T)
```

`TestSwitchingChangesWhichAppsAreVisible` must create an app in each of two teams and assert the app list changes across the switch. That is the end-to-end proof that `owner_id` scoping follows the session.

- [ ] **Steps 2-4.** `account.SwitchTeam` already verifies membership; the handler must not bypass it. The switcher renders through the existing `SlotProvider` so the engine's chrome stays the seam a wrapper replaces.

- [ ] **Step 5: `make generate && make css`, commit**

---

### Task 7: Team management page

**Files:** Create `internal/web/team.templ`, modify `internal/web/team.go`.

**Interfaces:** `GET /team`, `POST /team/invite`, `POST /team/invitations/{id}/revoke`, `POST /team/members/{id}/remove`, `POST /team/members/{id}/role`

- [ ] **Step 1: Write the failing test**

```go
func TestTeamPageListsMembersAndPendingInvitations(t *testing.T)
func TestInviteSendsMailAndShowsPending(t *testing.T)
// An admin must not be able to mint an owner — that is ownership.
func TestAdminCannotInviteAnOwner(t *testing.T)
func TestRevokedInvitationDisappearsAndStopsWorking(t *testing.T)
func TestRemovingAMemberEndsTheirSession(t *testing.T)
// A team with no owner cannot be administered by anyone, ever.
func TestCannotRemoveOrDemoteTheLastOwner(t *testing.T)
// Invitation tokens must not be rendered anywhere.
func TestPendingInvitationDoesNotLeakItsToken(t *testing.T)
```

`TestRemovingAMemberEndsTheirSession` is the HTTP-level proof of the fix made after B: it must assert the removed member's cookie stops working immediately.

`TestPendingInvitationDoesNotLeakItsToken` must assert the rendered HTML contains neither the raw token nor its hash. A management page that prints the invitation link lets any admin impersonate the invitee.

- [ ] **Steps 2-4: fail, implement, verify. Step 5: assets, commit**

---

### Task 8: Invitation accept flow, and cleanup on removal

**Files:** Modify `internal/web/auth.go`, `internal/account/account.go`. Create `internal/store/migrations/00005_invitation_cleanup.sql` only if a schema change proves necessary.

**Interfaces:** `GET /invitations/{token}` (outside the authenticated group)

- [ ] **Step 1: Write the failing test**

```go
func TestAcceptingAnInvitationWhileSignedOutSignsYouIn(t *testing.T)
func TestAcceptingAnInvitationWhileSignedInAddsTheTeam(t *testing.T)
// Carried over from B's adversarial review (LOW): a departing admin left
// themselves a self-service way back in.
func TestInvitationsDieWithTheirInviter(t *testing.T)
func TestAcceptedInvitationCannotBeReplayed(t *testing.T)
```

Accepting while signed out must **not** trust the token as proof of identity for an arbitrary address — B bound acceptance to the invited address, so the flow must establish who the person is (issue a magic link to the invited address) rather than signing in whoever holds the link.

- [ ] **Steps 2-4.** `RemoveMember` and demotion from Admin must delete that person's pending invitations for that team, in the same transaction as the membership change.

- [ ] **Step 5: Commit**

---

### Task 9: Wiring, settings page, and documentation

**Files:** Modify `cmd/yacht/main.go`, `internal/web/server.go`, `internal/web/pages.templ`, `.env.example`, `README.md`.

- [ ] **Step 1: Write the failing test**

```go
// The warning B shipped ("this build serves no sign-in page yet") must be
// gone, and its condition with it.
func TestNoStaleNoSignInPageWarning(t *testing.T)
func TestSettingsShowsAccountsPosture(t *testing.T)
// Unauthenticated requests must land on the sign-in page, not a bare 401.
func TestUnauthenticatedRequestRedirectsToSignIn(t *testing.T)
// ...except for API-ish paths, where a redirect would be a confusing 200.
func TestHealthzStaysOutsideAuth(t *testing.T)
```

- [ ] **Steps 2-4.** Remove the `log.Warn` in `newIdentity` about there being no sign-in page. Settings shows whether accounts are on, which mail transport is active, and the session TTL.

- [ ] **Step 5: Full verification and commit**

```bash
go vet ./...
CGO_ENABLED=0 go build ./...
go test ./... -race -count=1
make generate && make css
git diff --exit-code -- '*_templ.go' internal/web/assets/css/app.css
git status --short
```

---

## Final verification

- [ ] **Full standard**, as above, plus:

```bash
go test ./internal/store -v -count=1 | grep -c SKIP   # must be 0
git check-ignore -v $(git ls-files --others --exclude-standard) 2>/dev/null
```

- [ ] **Confirm sub-projects A and B still pass**

```bash
go test ./internal/app/ ./internal/domain/ ./internal/account/ -count=1
```

- [ ] **Confirm no route escaped a gate**

```bash
go test ./internal/web/ -run TestEveryMutatingRouteHasARoleGate -v
```

---

## Out of scope

- **TOTP enrolment** — columns exist, no flow.
- **Project-level permissions**, Slack/Discord/Telegram, alert rules.
- **Volumes** — the next sub-project.
