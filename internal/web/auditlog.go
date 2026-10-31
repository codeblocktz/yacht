package web

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/audit"
	"github.com/codeblocktz/yacht/internal/identity"
)

// AuditLog is the team's record of who changed what. An interface at the
// consumer, like Apps, so the dashboard can be tested without a database.
type AuditLog interface {
	Record(ctx context.Context, ownerID string, actor audit.Actor, ev audit.Event)
	List(ctx context.Context, ownerID string, f audit.Filter) ([]audit.Entry, error)
	Actors(ctx context.Context, ownerID string) ([]string, error)
}

var _ AuditLog = (*audit.Log)(nil)

// auditSpec says what one route records when it succeeds: the action, what it
// acted on, and the few named facts worth keeping. Every field is read from
// the URL or from a form field named here — never from the whole form —
// which is what keeps a variable's value or a typed confirmation out of the
// log by construction rather than by care.
type auditSpec struct {
	action     string
	targetKind string
	// target is the URL parameter or form field naming what was acted on;
	// targetFn computes it when that is not enough.
	target   string
	targetFn func(s *Server, r *http.Request) string
	// detail maps a name in the record to a URL parameter (":name") or a
	// form field ("name") to read it from.
	detail map[string]string
	// detailFn adds facts that need working out.
	detailFn func(r *http.Request) map[string]string
	// ownerParam names a URL parameter that is the team the event belongs
	// to, for the one action recorded in a team other than the one acting:
	// an operator starting to act as it.
	ownerParam string
	// when decides whether a successful request is an event at all — a read
	// of the variables is one only when it revealed secrets.
	when func(r *http.Request) bool
}

// auditRoutes is every action the log records, by method and route pattern.
// One table for the dashboard and the API, so the same change reads the same
// whichever way it was made. A route that is not here is not recorded, which
// is right for reads and for changes that only arrange the picture.
var auditRoutes = map[string]auditSpec{
	// Deploys.
	"POST /apps":                                {action: "app.create", targetKind: "app", target: "name", detail: map[string]string{"source": "source", "project": "project"}},
	"POST /apps/github":                         {action: "app.create", targetKind: "app", detail: map[string]string{"source": "=github", "kind": "=github", "project": "project"}},
	"POST /apps/new/database":                   {action: "app.create", targetKind: "app", detail: map[string]string{"engine": "engine", "kind": "=database", "project": "project"}},
	"POST /apps/new/bucket":                     {action: "app.create", targetKind: "app", detail: map[string]string{"kind": "=bucket", "project": "project"}},
	"POST /templates":                           {action: "app.create", targetKind: "template", target: "template", detail: map[string]string{"project": "project"}},
	"POST /apps/{name}/redeploy":                {action: "deploy.redeploy", targetKind: "app", target: ":name"},
	"POST /apps/{name}/changes/deploy":          {action: "deploy.staged", targetKind: "app", target: ":name"},
	"POST /projects/{slug}/changes/deploy":      {action: "deploy.staged", targetKind: "project", target: ":slug"},
	"POST /apps/{name}/rollback":                {action: "deploy.rollback", targetKind: "app", target: ":name", detail: map[string]string{"release": "release"}},
	"POST /apps/{name}/deployments/cancel":      {action: "deploy.cancel", targetKind: "app", target: ":name"},
	"POST /apps/{name}/scale":                   {action: "app.scale", targetKind: "app", target: ":name", detail: map[string]string{"replicas": "replicas"}},
	"POST /apps/{name}/runtime":                 {action: "app.settings", targetKind: "app", target: ":name"},
	"POST /apps/{name}/health":                  {action: "app.settings", targetKind: "app", target: ":name", detail: map[string]string{"health_path": "health_path"}},
	"POST /apps/{name}/region":                  {action: "app.region", targetKind: "app", target: ":name", detail: map[string]string{"region": "region"}},
	"POST /apps/{name}/project":                 {action: "app.move", targetKind: "app", target: ":name", detail: map[string]string{"project": "project"}},
	"POST /apps/{name}/delete":                  {action: "app.delete", targetKind: "app", target: ":name"},
	"POST /apps/{name}/source/disconnect":       {action: "app.source.disconnect", targetKind: "app", target: ":name"},
	"POST /apps/{name}/hook":                    {action: "app.hook.enable", targetKind: "app", target: ":name"},
	"POST /apps/{name}/hook/delete":             {action: "app.hook.disable", targetKind: "app", target: ":name"},
	"POST /apps/{name}/sleep":                   {action: "app.sleep", targetKind: "app", target: ":name", detail: map[string]string{"how": "=now"}},
	"POST /apps/{name}/wake":                    {action: "app.wake", targetKind: "app", target: ":name", detail: map[string]string{"how": "=now"}},
	"POST /apps/{name}/sleep/settings":          {action: "app.sleep", targetKind: "app", target: ":name"},
	"POST /apps/{name}/backups/{id}/restore":    {action: "storage.restore", targetKind: "app", target: ":name", detail: map[string]string{"backup": ":id"}},
	"POST /apps/{name}/backups/{id}/delete":     {action: "storage.backup.delete", targetKind: "app", target: ":name", detail: map[string]string{"backup": ":id"}},
	"POST /apps/{name}/database/reveal":         {action: "secret.reveal", targetKind: "app", target: ":name", detail: map[string]string{"what": "=connection string"}},
	"GET /apps/{name}/variables/{key}/value":    {action: "secret.reveal", targetKind: "app", target: ":name", detail: map[string]string{"key": ":key"}},
	"GET /projects/{slug}/variables/{key}/value": {action: "secret.reveal", targetKind: "project", target: ":slug", detail: map[string]string{"key": ":key"}},
	"GET /apps/{name}/console/ws":               {action: "console.session", targetKind: "app", target: ":name", detail: map[string]string{"pod": "pod"}},

	// Variables: keys only, ever.
	"POST /apps/{name}/variables":                 {action: "variable.set", targetKind: "app", target: ":name", detail: map[string]string{"key": "key"}, detailFn: secretFlag},
	"POST /apps/{name}/variables/raw":             {action: "variable.raw", targetKind: "app", target: ":name", detailFn: rawKeys},
	"POST /apps/{name}/variables/{key}/delete":    {action: "variable.delete", targetKind: "app", target: ":name", detail: map[string]string{"key": ":key"}},
	"POST /apps/{name}/changes/discard":           {action: "variable.discard", targetKind: "app", target: ":name"},
	"POST /projects/{slug}/variables":             {action: "variable.set", targetKind: "project", target: ":slug", detail: map[string]string{"key": "key"}, detailFn: secretFlag},
	"POST /projects/{slug}/variables/{key}/delete": {action: "variable.delete", targetKind: "project", target: ":slug", detail: map[string]string{"key": ":key"}},
	"POST /projects/{slug}/changes/discard":       {action: "variable.discard", targetKind: "project", target: ":slug"},

	// Domains.
	"POST /apps/{name}/domains":                       {action: "domain.add", targetKind: "app", target: ":name", detail: map[string]string{"host": "host"}},
	"POST /apps/{name}/domains/{id}/verify":           {action: "domain.verify", targetKind: "app", target: ":name", detail: map[string]string{"domain": ":id"}},
	"POST /apps/{name}/domains/{id}/delete":           {action: "domain.remove", targetKind: "app", target: ":name", detail: map[string]string{"domain": ":id"}},
	"POST /apps/{name}/networking":                    {action: "domain.routing", targetKind: "app", target: ":name"},
	"POST /apps/{name}/networking/domain/generate":    {action: "domain.generate", targetKind: "app", target: ":name"},
	"POST /apps/{name}/networking/domain":             {action: "domain.set", targetKind: "app", target: ":name", detail: map[string]string{"label": "label", "port": "port"}},
	"POST /apps/{name}/networking/domain/delete":      {action: "domain.remove", targetKind: "app", target: ":name", detail: map[string]string{"domain": "=generated"}},
	"POST /apps/{name}/networking/tcp-proxy":           {action: "domain.tcp-proxy.enable", targetKind: "app", target: ":name"},
	"POST /apps/{name}/networking/tcp-proxy/delete":    {action: "domain.tcp-proxy.disable", targetKind: "app", target: ":name"},

	// Storage.
	"POST /apps/{name}/storage":                   {action: "storage.attach", targetKind: "app", target: ":name", detail: map[string]string{"volume": "name", "mount": "mount_path"}},
	"POST /apps/{name}/storage/{volume}/resize":   {action: "storage.resize", targetKind: "app", target: ":name", detail: map[string]string{"volume": ":volume", "size_gb": "size_gb"}},
	"POST /apps/{name}/storage/{volume}/delete":   {action: "storage.delete", targetKind: "app", target: ":name", detail: map[string]string{"volume": ":volume"}},

	// A bucket's files, by name: an upload's are set by the handler, which
	// alone has read the multipart body.
	"POST /apps/{name}/files":        {action: "bucket.file.upload", targetKind: "app", target: ":name"},
	"POST /apps/{name}/files/delete": {action: "bucket.file.delete", targetKind: "app", target: ":name", detail: map[string]string{"key": "key"}},

	// Projects.
	"POST /projects":                {action: "project.create", targetKind: "project", target: "name"},
	"POST /projects/{slug}/rename":  {action: "project.rename", targetKind: "project", target: ":slug", detail: map[string]string{"name": "name"}},
	"POST /projects/{slug}/delete":  {action: "project.delete", targetKind: "project", target: ":slug"},

	// Environments. Deleting one deletes every service in it, so who did is
	// as much the log's as a project's deletion.
	"POST /projects/{slug}/environments":                {action: "environment.create", targetKind: "project", target: ":slug", detail: map[string]string{"name": "name"}, detailFn: environmentFrom},
	"POST /projects/{slug}/environments/{env}/delete":   {action: "environment.delete", targetKind: "project", target: ":slug", detail: map[string]string{"name": ":env"}},
	"POST /projects/{slug}/pr-environments":             {action: "environment.pr", targetKind: "project", target: ":slug", detailFn: prEnvironmentsState},

	// People.
	"POST /team/invite":                    {action: "invitation.create", targetKind: "member", target: "email", detail: map[string]string{"role": "role"}},
	"POST /team/invitations/{id}/revoke":   {action: "invitation.revoke", targetKind: "invitation", target: ":id"},
	"POST /team/members/{id}/remove":       {action: "member.remove", targetKind: "member", targetFn: memberTarget},
	"POST /team/members/{id}/role":         {action: "member.role", targetKind: "member", targetFn: memberTarget, detail: map[string]string{"role": "role"}},
	"POST /team/name":                      {action: "team.rename", targetKind: "team", target: "name"},
	"POST /team/sleep":                     {action: "team.sleep", targetKind: "team"},
	"POST /team/github/choose":             {action: "github.connect", targetKind: "team"},
	"POST /team/github/{id}/disconnect":    {action: "github.disconnect", targetKind: "github", target: ":id"},
	"POST /admin/teams/{id}/act":           {action: "acting.start", targetKind: "team", target: ":id", ownerParam: "id", detail: map[string]string{"reason": "reason"}},
	"POST /acting/stop":                    {action: "acting.stop", targetKind: "team"},

	// Tokens and webhooks.
	"POST /team/tokens":                                     {action: "token.create", targetKind: "token", target: "name", detail: map[string]string{"role": "role"}},
	"POST /team/tokens/{id}/revoke":                         {action: "token.revoke", targetKind: "token", target: ":id", detail: map[string]string{"token": ":id"}},
	"POST /projects/{slug}/tokens":                          {action: "token.create", targetKind: "token", target: "name", detail: map[string]string{"role": "role", "project": ":slug"}},
	"POST /projects/{slug}/tokens/{id}/revoke":              {action: "token.revoke", targetKind: "token", target: ":id", detail: map[string]string{"token": ":id", "project": ":slug"}},
	"POST /cli/authorize":                                   {action: "token.create", targetKind: "token", target: "=CLI", detail: map[string]string{"role": "role", "origin": "=cli"}, when: func(r *http.Request) bool { return r.FormValue("decision") == "approve" }},
	"POST /projects/{slug}/webhooks":                        {action: "webhook.create", targetKind: "project", target: ":slug", detailFn: webhookHost},
	"POST /projects/{slug}/webhooks/{id}/delete":            {action: "webhook.delete", targetKind: "project", target: ":slug", detail: map[string]string{"webhook": ":id"}},
	"POST /projects/{slug}/webhooks/{id}/test":              {action: "webhook.test", targetKind: "project", target: ":slug", detail: map[string]string{"webhook": ":id"}},

	// The API, recorded as the dashboard's are.
	"POST /api/v1/apps/{name}/redeploy":         {action: "deploy.redeploy", targetKind: "app", target: ":name"},
	"POST /api/v1/apps/{name}/deploy-staged":    {action: "deploy.staged", targetKind: "app", target: ":name"},
	"POST /api/v1/apps/{name}/rollback":         {action: "deploy.rollback", targetKind: "app", target: ":name"},
	"POST /api/v1/apps/{name}/up":               {action: "deploy.upload", targetKind: "app", target: ":name"},
	"PUT /api/v1/apps/{name}/variables/{key}":   {action: "variable.set", targetKind: "app", target: ":name", detail: map[string]string{"key": ":key"}},
	"DELETE /api/v1/apps/{name}/variables/{key}": {action: "variable.delete", targetKind: "app", target: ":name", detail: map[string]string{"key": ":key"}},
	"GET /api/v1/apps/{name}/variables": {action: "secret.reveal", targetKind: "app", target: ":name", detail: map[string]string{"what": "=all secrets"},
		when: func(r *http.Request) bool { return revealRequested(r) }},
}

// auditState travels with one request, so a handler that reports failure by
// flash rather than by status — the dashboard's way — is not recorded as
// having succeeded.
type auditState struct {
	failed bool
	// target is what the handler says it acted on, for a route whose URL
	// and form cannot say: a database's name is chosen by the server, and a
	// revoked token is gone from the list by the time anyone reads its id.
	target string
	// detail is facts the handler adds, for the same reason: the names of
	// the files an upload stored are in a multipart body only it has read.
	detail map[string]string
}

type auditStateKey struct{}

// markAuditFailed is called by setFlash for an error: the action the request
// asked for did not happen, whatever status the redirect carries.
func markAuditFailed(ctx context.Context) {
	if st, ok := ctx.Value(auditStateKey{}).(*auditState); ok {
		st.failed = true
	}
}

// auditTarget names what a request acted on, for the log, when the table's
// URL parameter or form field cannot. It wins over the table's target.
func auditTarget(ctx context.Context, target string) {
	if st, ok := ctx.Value(auditStateKey{}).(*auditState); ok {
		st.target = target
	}
}

// auditDetail adds a fact to what a request is recorded with, when the
// table's URL parameters and form fields cannot read it. It wins over the
// table's.
func auditDetail(ctx context.Context, key, value string) {
	if st, ok := ctx.Value(auditStateKey{}).(*auditState); ok {
		if st.detail == nil {
			st.detail = map[string]string{}
		}
		st.detail[key] = value
	}
}

// auditTrail records the actions in auditRoutes that succeed. Mounted on the
// authenticated groups after identity, like the role gates, so a route added
// to the table is recorded whoever handles it.
func (s *Server) auditTrail(next http.Handler) http.Handler {
	if s.audit == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Parsed now, so the fields named in the table can be read after the
		// handler has run: a handler given a copy of the request parses its
		// own, and the body is gone by the time this one could.
		if r.Method == http.MethodPost && strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			_ = r.ParseForm()
		}
		st := &auditState{}
		r = r.WithContext(context.WithValue(r.Context(), auditStateKey{}, st))
		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		rctx := chi.RouteContext(r.Context())
		if rctx == nil {
			return
		}
		spec, ok := auditRoutes[r.Method+" "+rctx.RoutePattern()]
		if !ok || st.failed {
			return
		}
		// 101 is a console socket opened; anything 4xx or worse did not
		// happen.
		if code := ww.Status(); code >= 400 {
			return
		}
		if spec.when != nil && !spec.when(r) {
			return
		}
		owner, ok := identity.FromContext(r.Context())
		if !ok {
			return
		}
		ownerID := owner.ID
		if spec.ownerParam != "" {
			ownerID = chi.URLParam(r, spec.ownerParam)
		}
		s.audit.Record(r.Context(), ownerID, s.auditActor(r), s.auditEvent(r, spec))
	})
}

// auditEvent builds the record for one request from its spec.
func (s *Server) auditEvent(r *http.Request, spec auditSpec) audit.Event {
	ev := audit.Event{Action: spec.action, TargetKind: spec.targetKind, Detail: map[string]string{}}
	if spec.targetFn != nil {
		ev.Target = spec.targetFn(s, r)
	} else if spec.target != "" {
		ev.Target = auditField(r, spec.target)
	}
	if st, ok := r.Context().Value(auditStateKey{}).(*auditState); ok && st.target != "" {
		ev.Target = st.target
	}
	for k, from := range spec.detail {
		if v := auditField(r, from); v != "" {
			ev.Detail[k] = v
		}
	}
	if spec.detailFn != nil {
		for k, v := range spec.detailFn(r) {
			ev.Detail[k] = v
		}
	}
	if st, ok := r.Context().Value(auditStateKey{}).(*auditState); ok {
		for k, v := range st.detail {
			ev.Detail[k] = v
		}
	}
	return ev
}

// auditField reads ":param" from the URL, "=literal" as itself, and anything
// else from the form by that one name.
func auditField(r *http.Request, from string) string {
	switch {
	case strings.HasPrefix(from, ":"):
		return chi.URLParam(r, from[1:])
	case strings.HasPrefix(from, "="):
		return from[1:]
	}
	return strings.TrimSpace(r.FormValue(from))
}

// auditActor is who made a request and from where: the token, for the API;
// the person, for the dashboard — marked as the operator when they are acting
// as the team rather than a member of it.
func (s *Server) auditActor(r *http.Request) audit.Actor {
	act := audit.Actor{IP: clientIP(r), UserAgent: r.UserAgent()}
	if p, ok := apiPrincipalFrom(r.Context()); ok {
		if p.token != nil {
			act.Kind, act.ID, act.Name = audit.ActorToken, p.token.TokenID.String(), p.token.TokenName
			return act
		}
	}
	owner, _ := identity.FromContext(r.Context())
	if s.accounts != nil {
		if sess, err := s.accounts.ResolveSession(r.Context(), sessionToken(r)); err == nil {
			act.Kind, act.ID = audit.ActorUser, sess.UserID.String()
			if u, err := s.accounts.User(r.Context(), sess.UserID); err == nil {
				act.Name = u.Email
			}
			if sess.ActingTeamID != "" && sess.ActingTeamID == owner.ID {
				act.Kind = audit.ActorOperator
			}
			return act
		}
	}
	// An install with no accounts has one principal, and it is the owner.
	act.Kind, act.Name = audit.ActorUser, displayName(owner)
	return act
}

// secretFlag records whether a variable set was a secret — the fact, never
// the value.
func secretFlag(r *http.Request) map[string]string {
	if formChecked(r, "secret") {
		return map[string]string{"secret": "yes"}
	}
	return nil
}

// rawKeys is the keys the Raw Editor saved, read from the text with the same
// parser the save used and keeping nothing else of it.
func rawKeys(r *http.Request) map[string]string {
	lines, err := app.ParseEnv(r.FormValue("raw"))
	if err != nil || len(lines) == 0 {
		return nil
	}
	keys := make([]string, 0, len(lines))
	for _, l := range lines {
		keys = append(keys, l.Key)
	}
	sort.Strings(keys)
	if len(keys) > 20 {
		keys = append(keys[:20], "…")
	}
	return map[string]string{"keys": strings.Join(keys, ", "), "count": strconv.Itoa(len(lines))}
}

// environmentFrom is the environment a new one copied, as environmentCreate
// reads the field: none for "-", production for nothing.
func environmentFrom(r *http.Request) map[string]string {
	switch from := strings.TrimSpace(r.FormValue("from")); from {
	case "-":
		return map[string]string{"from": "empty"}
	case "":
		return map[string]string{"from": app.DefaultEnvironment}
	default:
		return map[string]string{"from": from}
	}
}

// prEnvironmentsState is whether PR Environments were turned on or off.
func prEnvironmentsState(r *http.Request) map[string]string {
	if formChecked(r, "enabled") {
		return map[string]string{"state": "on"}
	}
	return map[string]string{"state": "off"}
}

// webhookHost records where a webhook sends, by host alone: Discord's and
// Slack's carry their credential in the path.
func webhookHost(r *http.Request) map[string]string {
	u := strings.TrimSpace(r.FormValue("url"))
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	return map[string]string{"host": u, "events": strings.Join(r.Form["events"], ", ")}
}

// memberTarget names a member by address rather than by id, read from the
// account service: the person still exists after being removed from a team.
func memberTarget(s *Server, r *http.Request) string {
	id := chi.URLParam(r, "id")
	if s.accounts != nil {
		if uid, err := uuid.Parse(id); err == nil {
			if u, err := s.accounts.User(r.Context(), uid); err == nil {
				return u.Email
			}
		}
	}
	return id
}

// TeamAuditData is Team › Audit log.
type TeamAuditData struct {
	TeamName string
	Entries  []audit.Entry
	Filter   audit.Filter
	// Category and Actor are the filters as chosen, for the form.
	Category, Actor, Query, Since, Until string
	Actors     []string
	Categories []string
	// Query string for the next page, with the filters kept.
	NextQuery string
	Tabs      TeamTabs
}

// teamAudit draws the team's audit log. Admin: the log names who did what to
// everything the team has, and a member who could read it could read who
// revealed which secret.
func (s *Server) teamAudit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	q := r.URL.Query()
	d := TeamAuditData{
		TeamName: displayName(owner), Category: q.Get("category"), Actor: q.Get("actor"),
		Query: q.Get("q"), Since: q.Get("since"), Until: q.Get("until"), Categories: audit.Categories,
		Tabs: s.teamTabs(r, "audit"),
	}
	f := audit.Filter{Category: d.Category, Actor: d.Actor, Query: d.Query, Limit: 50}
	if t, err := time.Parse("2006-01-02", d.Since); err == nil {
		f.Since = t
	}
	if t, err := time.Parse("2006-01-02", d.Until); err == nil {
		f.Until = t.Add(24 * time.Hour)
	}
	if t, err := time.Parse(time.RFC3339Nano, q.Get("before")); err == nil {
		f.Before = t
	}
	entries, err := s.audit.List(ctx, owner.ID, f)
	if err != nil {
		s.log.Error("read the audit log", slog.String("error", err.Error()))
		http.Error(w, "could not read the audit log", http.StatusInternalServerError)
		return
	}
	d.Entries, d.Filter = entries, f
	if len(entries) == f.Limit {
		next := q
		next.Set("before", entries[len(entries)-1].At.Format(time.RFC3339Nano))
		d.NextQuery = next.Encode()
	}
	if actors, err := s.audit.Actors(ctx, owner.ID); err == nil {
		d.Actors = actors
	}
	s.render(w, r, TeamAuditPage(d))
}
