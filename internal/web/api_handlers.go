package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/apitoken"
	"github.com/codeblocktz/yacht/internal/identity"
)

// uploader is what `up` needs of the app service beyond Apps. Asserted like
// the variables tab's extras, so a wrapper's own Apps keeps compiling and
// simply has no `up`.
type uploader interface {
	DeployUpload(ctx context.Context, ownerID, appName string, archive []byte) (app.Deployment, app.Upload, error)
	MaxUploadBytes() int64
	SourceArchive(ctx context.Context, id uuid.UUID, token string) ([]byte, error)
	UploadForDeployment(ctx context.Context, ownerID string, deployID uuid.UUID) (app.Upload, error)
}

// The API's view of things. Field names are what a CLI and a CI script read,
// so they are spelled for them — snake_case, ids as strings — and they are
// the API's contract: added to, never renamed.
type (
	apiMeView struct {
		Team    apiTeamView     `json:"team"`
		Role    string          `json:"role"`
		Token   *apiTokenView   `json:"token,omitempty"`
		Project *apiProjectView `json:"project,omitempty"`
	}
	apiTeamView struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	apiTokenView struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		CreatedBy string `json:"created_by"`
	}
	apiProjectView struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	apiAppView struct {
		ID         string             `json:"id"`
		Name       string             `json:"name"`
		Project    string             `json:"project_id,omitempty"`
		Source     string             `json:"source"`
		Image      string             `json:"image"`
		Replicas   int32              `json:"replicas"`
		Port       int32              `json:"port"`
		URL        string             `json:"url,omitempty"`
		Region     string             `json:"region,omitempty"`
		Status     apiStatusView      `json:"status"`
		LastDeploy string             `json:"last_deploy,omitempty"`
		Dashboard  string             `json:"dashboard_url,omitempty"`
		Latest     *apiDeploymentView `json:"latest_deployment,omitempty"`
		CreatedAt  time.Time          `json:"created_at"`
	}
	apiStatusView struct {
		Known   bool   `json:"known"`
		Phase   string `json:"phase"`
		Ready   int32  `json:"ready"`
		Desired int32  `json:"desired"`
		Message string `json:"message,omitempty"`
	}
	apiDeploymentView struct {
		ID         string     `json:"id"`
		Title      string     `json:"title"`
		Badge      string     `json:"badge"`
		Status     string     `json:"status"`
		Source     string     `json:"source"`
		Trigger    string     `json:"trigger"`
		Actor      string     `json:"actor"`
		Image      string     `json:"image"`
		Active     bool       `json:"active"`
		Message    string     `json:"message,omitempty"`
		StartedAt  time.Time  `json:"started_at"`
		FinishedAt *time.Time `json:"finished_at,omitempty"`
		Build      *apiBuild  `json:"build,omitempty"`
		Upload     *apiUpload `json:"upload,omitempty"`
	}
	apiBuild struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Commit  string `json:"commit,omitempty"`
		Message string `json:"message,omitempty"`
	}
	apiUpload struct {
		ID     string `json:"id"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	}
	apiVariableView struct {
		Key       string `json:"key"`
		Value     string `json:"value"`
		Secret    bool   `json:"secret"`
		Masked    bool   `json:"masked,omitempty"`
		Reference bool   `json:"reference,omitempty"`
		Staged    string `json:"staged,omitempty"`
	}
)

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	owner := identity.MustFromContext(r.Context())
	p, _ := apiPrincipalFrom(r.Context())
	v := apiMeView{Team: apiTeamView{ID: owner.ID, Name: displayName(owner)}, Role: string(p.role)}
	if p.token != nil {
		v.Token = &apiTokenView{ID: p.token.TokenID.String(), Name: p.token.TokenName, CreatedBy: p.token.MakerEmail}
		if p.token.ProjectID != nil {
			v.Project = &apiProjectView{ID: p.token.ProjectID.String(), Slug: p.token.ProjectSlug}
		}
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) apiProjects(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	projects, err := s.apps.Projects(ctx, owner.ID)
	if err != nil {
		s.apiFailed(w, r, err)
		return
	}
	only := apiProject(ctx)
	out := []apiProjectView{}
	for _, p := range projects {
		if only != nil && p.ID != *only {
			continue
		}
		out = append(out, apiProjectView{ID: p.ID.String(), Slug: p.Slug, Name: p.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

func (s *Server) apiProjectApps(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	p, err := s.apps.Project(ctx, owner.ID, chi.URLParam(r, "slug"))
	if err != nil || (apiProject(ctx) != nil && p.ID != *apiProject(ctx)) {
		writeAPIError(w, http.StatusNotFound, "not_found", "no such project")
		return
	}
	apps, err := s.apps.ListInProject(ctx, owner.ID, p.ID)
	if err != nil {
		s.apiFailed(w, r, err)
		return
	}
	s.writeApps(w, r, apps)
}

func (s *Server) apiApps(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	var apps []app.App
	var err error
	if only := apiProject(ctx); only != nil {
		apps, err = s.apps.ListInProject(ctx, owner.ID, *only)
	} else {
		apps, err = s.apps.List(ctx, owner.ID)
	}
	if err != nil {
		s.apiFailed(w, r, err)
		return
	}
	s.writeApps(w, r, apps)
}

func (s *Server) writeApps(w http.ResponseWriter, r *http.Request, apps []app.App) {
	out := make([]apiAppView, 0, len(apps))
	for _, a := range apps {
		out = append(out, s.appView(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": out})
}

func (s *Server) appView(a app.App) apiAppView {
	v := apiAppView{
		ID: a.ID.String(), Name: a.Name, Source: string(a.Source), Image: a.Image,
		Replicas: a.Replicas, Port: a.Port, Region: a.Region, LastDeploy: a.LastDeploy,
		CreatedAt: a.CreatedAt,
		Status: apiStatusView{
			Known: a.StatusKnown, Phase: string(a.Status.Phase), Ready: a.Status.Ready,
			Desired: a.Status.Desired, Message: a.Status.Message,
		},
	}
	if a.ProjectID != uuid.Nil {
		v.Project = a.ProjectID.String()
	}
	if a.Host != "" {
		v.URL = a.URLScheme() + "://" + a.Host
	}
	if s.baseURL != "" {
		v.Dashboard = s.baseURL + "/apps/" + a.Name
	}
	return v
}

// apiApp is the app a request names, as long as the token may reach it. A
// project token asking for an app of another project is told it does not
// exist, as a token from another team is: what it cannot reach is not its
// business to know about.
func (s *Server) apiApp(w http.ResponseWriter, r *http.Request) (app.App, bool) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	a, err := s.apps.Get(ctx, owner.ID, chi.URLParam(r, "name"))
	if err == nil {
		if only := apiProject(ctx); only == nil || a.ProjectID == *only {
			return a, true
		}
	}
	if err != nil && !errors.Is(err, app.ErrNotFound) {
		s.apiFailed(w, r, err)
		return app.App{}, false
	}
	writeAPIError(w, http.StatusNotFound, "not_found", "no such app")
	return app.App{}, false
}

func (s *Server) apiAppGet(w http.ResponseWriter, r *http.Request) {
	a, ok := s.apiApp(w, r)
	if !ok {
		return
	}
	v := s.appView(a)
	if deps, err := s.apps.Deployments(r.Context(), a.OwnerID, a.ID, 1); err == nil && len(deps) > 0 {
		d := s.deploymentView(r.Context(), a, deps[0])
		v.Latest = &d
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) deploymentView(ctx context.Context, a app.App, d app.Deployment) apiDeploymentView {
	badge, _ := deploymentBadge(d)
	v := apiDeploymentView{
		ID: d.ID.String(), Title: deploymentTitle(d), Badge: badge, Status: d.Status,
		Source: deploymentSource(a, d), Trigger: d.Trigger, Actor: d.ActorKind,
		Image: d.Image, Active: d.IsActive, Message: failureReason(d.Message),
		StartedAt: d.StartedAt, FinishedAt: d.FinishedAt,
	}
	if d.ActorID != "" {
		v.Actor = d.ActorKind + ":" + d.ActorID
	}
	if up, ok := s.apps.(uploader); ok {
		if u, err := up.UploadForDeployment(ctx, a.OwnerID, d.ID); err == nil {
			v.Upload = &apiUpload{ID: u.ID.String(), Bytes: u.SizeBytes, SHA256: u.SHA256}
		}
	}
	return v
}

func (s *Server) apiDeployments(w http.ResponseWriter, r *http.Request) {
	a, ok := s.apiApp(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	deps, err := s.apps.Deployments(r.Context(), a.OwnerID, a.ID, int32(limit))
	if err != nil {
		s.apiFailed(w, r, err)
		return
	}
	out := make([]apiDeploymentView, 0, len(deps))
	for _, d := range deps {
		out = append(out, s.deploymentView(r.Context(), a, d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": out})
}

// apiDeploymentOf is one of the app's deployments, by the id in the path.
func (s *Server) apiDeploymentOf(w http.ResponseWriter, r *http.Request, a app.App) (app.Deployment, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil || s.logs == nil {
		writeAPIError(w, http.StatusNotFound, "not_found", "no such deployment")
		return app.Deployment{}, false
	}
	d, err := s.logs.Deployment(r.Context(), a.OwnerID, a.Name, id)
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "no such deployment")
		} else {
			s.apiFailed(w, r, err)
		}
		return app.Deployment{}, false
	}
	return d, true
}

func (s *Server) apiDeployment(w http.ResponseWriter, r *http.Request) {
	a, ok := s.apiApp(w, r)
	if !ok {
		return
	}
	d, ok := s.apiDeploymentOf(w, r, a)
	if !ok {
		return
	}
	v := s.deploymentView(r.Context(), a, d)
	if b, err := s.logs.BuildForDeployment(r.Context(), a.OwnerID, d.ID); err == nil {
		v.Build = &apiBuild{ID: b.ID.String(), Status: b.Status, Commit: b.CommitSHA, Message: b.Message}
	}
	writeJSON(w, http.StatusOK, v)
}

// apiBuildLog is a deployment's build log as plain text. With follow, it
// stays open and sends what the build writes as it writes it, ending when
// the build does — what `up` prints while it waits.
func (s *Server) apiBuildLog(w http.ResponseWriter, r *http.Request) {
	a, ok := s.apiApp(w, r)
	if !ok {
		return
	}
	d, ok := s.apiDeploymentOf(w, r, a)
	if !ok {
		return
	}
	follow := r.URL.Query().Get("follow") == "1" || r.URL.Query().Get("follow") == "true"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	ctx, cancel := context.WithTimeout(r.Context(), maxStreamAge)
	defer cancel()
	sent := 0
	for {
		b, err := s.logs.BuildForDeployment(ctx, a.OwnerID, d.ID)
		if err == nil && len(b.Log) > sent {
			_, _ = io.WriteString(w, b.Log[sent:])
			sent = len(b.Log)
			if flusher != nil {
				flusher.Flush()
			}
		}
		// Done when the build is, or — for a deploy that has not started
		// building yet — when the deploy itself has finished without one.
		finished := err == nil && !b.Running()
		if !finished && errors.Is(err, app.ErrNotFound) {
			if cur, derr := s.logs.Deployment(ctx, a.OwnerID, a.Name, d.ID); derr == nil && cur.FinishedAt != nil {
				finished = true
			}
		}
		if !follow || finished {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// revealRequested is a variables read asking for secrets' values.
func revealRequested(r *http.Request) bool {
	v := r.URL.Query().Get("reveal")
	return v == "1" || v == "true"
}

// apiVariables lists an app's variables. A secret's value is masked unless
// an admin's token asks for it with reveal — the dashboard opens one value
// at a time and logs each; the API opens them together, and that one read is
// what the audit log records.
func (s *Server) apiVariables(w http.ResponseWriter, r *http.Request) {
	a, ok := s.apiApp(w, r)
	if !ok {
		return
	}
	reveal := revealRequested(r)
	if reveal {
		if role, ok := s.roleOf(r); !ok || !role.AtLeast(account.RoleAdmin) {
			writeAPIError(w, http.StatusForbidden, "forbidden",
				"only an admin's token can read secret values")
			return
		}
	}
	rv, canReveal := s.apps.(variableRevealer)
	out := make([]apiVariableView, 0, len(a.Variables))
	for _, v := range a.Variables {
		item := apiVariableView{Key: v.Key, Value: v.Value, Secret: v.Secret, Reference: v.Reference, Staged: v.Staged}
		if v.Secret {
			item.Value, item.Masked = "", true
			if reveal && canReveal {
				value, err := rv.RevealVariable(r.Context(), a.OwnerID, a.Name, v.Key)
				if err != nil {
					s.apiFailed(w, r, err)
					return
				}
				item.Value, item.Masked = value, false
			}
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"variables": out})
}

func (s *Server) apiVariableSet(w http.ResponseWriter, r *http.Request) {
	a, ok := s.apiApp(w, r)
	if !ok {
		return
	}
	var body struct {
		Value  *string `json:"value"`
		Secret bool    `json:"secret"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || body.Value == nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", `send {"value": "...", "secret": false}`)
		return
	}
	err := s.apps.SetVariable(r.Context(), a.OwnerID, a.Name, app.VariableInput{
		Key: chi.URLParam(r, "key"), Value: *body.Value, Secret: body.Secret,
	})
	if err != nil && !errors.Is(err, app.ErrSavedWhileDeploying) {
		s.apiFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"key": chi.URLParam(r, "key"), "saved": true, "staged": s.stages(),
		"deploying_later": errors.Is(err, app.ErrSavedWhileDeploying),
	})
}

func (s *Server) apiVariableDelete(w http.ResponseWriter, r *http.Request) {
	a, ok := s.apiApp(w, r)
	if !ok {
		return
	}
	if err := s.apps.DeleteVariable(r.Context(), a.OwnerID, a.Name, chi.URLParam(r, "key")); err != nil &&
		!errors.Is(err, app.ErrSavedWhileDeploying) {
		s.apiFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": chi.URLParam(r, "key"), "deleted": true, "staged": s.stages()})
}

// latestDeployment answers a deploy that was admitted with the deployment it
// made, which the service does not return: the newest one, read straight
// after.
func (s *Server) latestDeployment(w http.ResponseWriter, r *http.Request, a app.App) {
	deps, err := s.apps.Deployments(r.Context(), a.OwnerID, a.ID, 1)
	if err != nil || len(deps) == 0 {
		writeJSON(w, http.StatusAccepted, map[string]any{"deployment": nil})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"deployment": s.deploymentView(r.Context(), a, deps[0])})
}

func (s *Server) apiRedeploy(w http.ResponseWriter, r *http.Request) {
	a, ok := s.apiApp(w, r)
	if !ok {
		return
	}
	if err := s.apps.Redeploy(r.Context(), a.OwnerID, a.Name); err != nil {
		s.apiFailed(w, r, err)
		return
	}
	s.latestDeployment(w, r, a)
}

func (s *Server) apiDeployStaged(w http.ResponseWriter, r *http.Request) {
	a, ok := s.apiApp(w, r)
	if !ok {
		return
	}
	st, ok := s.apps.(changeStager)
	if !ok {
		writeAPIError(w, http.StatusNotFound, "not_found", "this install does not stage changes")
		return
	}
	if err := st.DeployStaged(r.Context(), a.OwnerID, a.Name); err != nil {
		s.apiFailed(w, r, err)
		return
	}
	s.latestDeployment(w, r, a)
}

// apiRollback puts the app back on the release one of its deployments ran:
// by deployment, since that is the id a person reads off the history, or by
// release.
func (s *Server) apiRollback(w http.ResponseWriter, r *http.Request) {
	a, ok := s.apiApp(w, r)
	if !ok {
		return
	}
	var body struct {
		Deployment string `json:"deployment_id"`
		Release    string `json:"release_id"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	releaseID, err := uuid.Parse(body.Release)
	if err != nil {
		depID, derr := uuid.Parse(body.Deployment)
		if derr != nil {
			writeAPIError(w, http.StatusBadRequest, "bad_request", `send {"deployment_id": "..."}`)
			return
		}
		deps, lerr := s.apps.Deployments(r.Context(), a.OwnerID, a.ID, 100)
		if lerr != nil {
			s.apiFailed(w, r, lerr)
			return
		}
		found := false
		for _, d := range deps {
			if d.ID == depID && d.ReleaseID != nil {
				releaseID, found = *d.ReleaseID, true
			}
		}
		if !found {
			writeAPIError(w, http.StatusNotFound, "not_found", "no such deployment with a release to roll back to")
			return
		}
	}
	if err := s.apps.Rollback(r.Context(), a.OwnerID, a.Name, releaseID); err != nil {
		s.apiFailed(w, r, err)
		return
	}
	s.latestDeployment(w, r, a)
}

// apiUp takes a gzipped tar of somebody's directory and builds and deploys
// it: `up`. The body is read to the limit and one byte more, so an archive
// over it is refused as too large rather than cut short and built broken.
func (s *Server) apiUp(w http.ResponseWriter, r *http.Request) {
	up, ok := s.apps.(uploader)
	if !ok {
		writeAPIError(w, http.StatusNotFound, "not_found", "this install does not take uploads")
		return
	}
	a, ok := s.apiApp(w, r)
	if !ok {
		return
	}
	limit := up.MaxUploadBytes()
	archive, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "the upload did not arrive whole")
		return
	}
	if int64(len(archive)) > limit {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "too_large",
			"the upload is over "+humanBytes(limit)+" — check .gitignore or .dockerignore leaves out dependencies and build output")
		return
	}
	d, u, err := up.DeployUpload(r.Context(), a.OwnerID, a.Name, archive)
	if err != nil {
		s.apiFailed(w, r, err)
		return
	}
	v := s.deploymentView(r.Context(), a, d)
	v.Upload = &apiUpload{ID: u.ID.String(), Bytes: u.SizeBytes, SHA256: u.SHA256}
	writeJSON(w, http.StatusAccepted, map[string]any{"deployment": v})
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatInt(n>>30, 10) + " GiB"
	case n >= 1<<20:
		return strconv.FormatInt(n>>20, 10) + " MiB"
	case n >= 1<<10:
		return strconv.FormatInt(n>>10, 10) + " KiB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}

// apiLogs is an app's container output: the last lines, or with follow a
// stream that stays open, one JSON object a line.
func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request) {
	a, ok := s.apiApp(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxStreamAge)
	defer cancel()
	q := r.URL.Query()
	tail, _ := strconv.ParseInt(q.Get("tail"), 10, 64)
	req := app.LogRequest{Pod: q.Get("pod")}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	enc := json.NewEncoder(w)
	type line struct {
		At   time.Time `json:"at"`
		Pod  string    `json:"pod,omitempty"`
		Text string    `json:"text"`
	}
	if q.Get("follow") != "1" && q.Get("follow") != "true" {
		req.Tail = tail
		l, err := s.logs.Logs(ctx, a.OwnerID, a.Name, req)
		if err != nil {
			s.apiFailed(w, r, err)
			return
		}
		for _, ln := range l.Lines {
			_ = enc.Encode(line{At: ln.At, Pod: l.Pod, Text: ln.Text})
		}
		return
	}
	lines, err := s.logs.LogStream(ctx, a.OwnerID, a.Name, req)
	if err != nil {
		s.apiFailed(w, r, err)
		return
	}
	flusher, _ := w.(http.Flusher)
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}
	bw := bufio.NewWriter(w)
	for ln, err := range lines {
		if err != nil {
			return
		}
		_ = json.NewEncoder(bw).Encode(line{At: ln.At, Pod: req.Pod, Text: ln.Text})
		_ = bw.Flush()
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// apiBuildSource serves an upload to the build that was started for it.
//
// Outside the token-authenticated group: a build pod has no API token and no
// team. What it has is the credential minted when its build started, in a
// Secret only its first step mounts, and that credential is good for this
// one archive for as long as a build can take. Every refusal is the same 404,
// so the endpoint says nothing about which uploads exist.
func (s *Server) apiBuildSource(w http.ResponseWriter, r *http.Request) {
	up, ok := s.apps.(uploader)
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	raw, hasToken := bearer(r)
	if !ok || err != nil || !hasToken || !s.apiFailures.allowed(clientIP(r)) {
		writeAPIError(w, http.StatusNotFound, "not_found", "no such source")
		return
	}
	archive, err := up.SourceArchive(r.Context(), id, raw)
	if err != nil {
		if !errors.Is(err, app.ErrUploadFetchDenied) {
			s.log.Error("serve uploaded source", slog.String("error", err.Error()))
		}
		s.apiFailures.fail(clientIP(r))
		writeAPIError(w, http.StatusNotFound, "not_found", "no such source")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
	_, _ = w.Write(archive)
}

// apiCLILoginStart opens a browser login for a CLI.
func (s *Server) apiCLILoginStart(w http.ResponseWriter, r *http.Request) {
	if !s.signInLimit.allow("cli:"+clientIP(r), 10) {
		writeAPIError(w, http.StatusTooManyRequests, "rate_limited", "too many logins started from this address — wait a minute")
		return
	}
	var body struct {
		Client string `json:"client"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	device, user, expires, err := s.tokens.StartLogin(r.Context(), body.Client)
	if err != nil {
		s.apiFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":      device,
		"user_code":        user,
		"verification_url": s.baseURL + "/cli/authorize?code=" + user,
		"expires_at":       expires,
		"interval":         2,
	})
}

// apiCLILoginPoll is the CLI waiting for its login to be approved.
func (s *Server) apiCLILoginPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	raw, tok, err := s.tokens.PollLogin(r.Context(), strings.TrimSpace(body.DeviceCode))
	switch {
	case errors.Is(err, apitoken.ErrLoginPending):
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending"})
	case errors.Is(err, apitoken.ErrLoginDenied):
		writeAPIError(w, http.StatusForbidden, "denied", "the login was refused in the browser")
	case errors.Is(err, apitoken.ErrLoginExpired):
		writeAPIError(w, http.StatusGone, "expired", "the login expired — run login again")
	case err != nil:
		s.apiFailed(w, r, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "approved", "token": raw,
			"token_id": tok.ID.String(), "team_id": tok.OwnerID, "role": string(tok.Role),
		})
	}
}

// apiFailed answers an error from a service the way the dashboard words it,
// with a status a program can act on.
func (s *Server) apiFailed(w http.ResponseWriter, r *http.Request, err error) {
	var upErr app.UploadError
	var rebuild app.UploadRebuildError
	switch {
	case errors.Is(err, app.ErrNotFound), errors.Is(err, app.ErrVariableNotFound),
		errors.Is(err, app.ErrProjectNotFound):
		writeAPIError(w, http.StatusNotFound, "not_found", failureReason(err.Error()))
	case errors.Is(err, app.ErrOperationInFlight):
		writeAPIError(w, http.StatusConflict, "deploy_in_flight",
			"a deploy is already in flight — wait for it to finish, or cancel it")
	case errors.Is(err, app.ErrReleaseActive):
		writeAPIError(w, http.StatusConflict, "already_active", "that release is already the one running")
	case errors.Is(err, app.ErrNoBuilder), errors.Is(err, app.ErrUploadNotSource), errors.Is(err, app.ErrNothingToBuild),
		errors.Is(err, app.ErrUploadUnfetchable), errors.Is(err, app.ErrNoSecretKey):
		writeAPIError(w, http.StatusConflict, "unavailable", failureReason(err.Error()))
	case errors.As(err, &rebuild):
		writeAPIError(w, http.StatusConflict, "needs_upload", rebuild.Explain(s.cliName))
	case errors.Is(err, app.ErrUploadTooLarge):
		writeAPIError(w, http.StatusRequestEntityTooLarge, "too_large", failureReason(err.Error()))
	case errors.As(err, &upErr), errors.As(err, new(app.MissingSecretsError)):
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid", failureReason(err.Error()))
	default:
		// The dashboard shows these on the page, and so does the API; the
		// log has them whole.
		s.log.Warn("api request failed", slog.String("path", r.URL.Path), slog.String("error", err.Error()))
		writeAPIError(w, http.StatusUnprocessableEntity, "failed", failureReason(err.Error()))
	}
}
