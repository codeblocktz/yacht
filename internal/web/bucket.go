package web

import (
	"context"
	"errors"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	objstore "github.com/codeblocktz/yacht/internal/bucket"
	"github.com/codeblocktz/yacht/internal/identity"
)

// Buckets is Add › Bucket and a bucket's Files tab.
//
// Optional, asserted on Apps like Databases: an install whose Apps does not
// offer it has no buckets.
type Buckets interface {
	DeployBucket(ctx context.Context, ownerID string, projectID uuid.UUID) (app.App, error)
	Bucket(ctx context.Context, ownerID, name string) (app.Bucket, error)
	BucketFiles(ctx context.Context, ownerID, name string) (*objstore.Files, error)
	MeasureBucket(ctx context.Context, ownerID, name string) error
	// PrepareBucketUpload gives a bucket about to be written its part of the
	// store's free room.
	PrepareBucketUpload(ctx context.Context, ownerID, name string) error
}

func (s *Server) buckets() Buckets {
	b, _ := s.apps.(Buckets)
	return b
}

// canBucket says the install has an object store, so Add › Bucket is
// offered.
func (s *Server) canBucket(ctx context.Context) bool {
	if s.buckets() == nil {
		return false
	}
	c, ok := s.apps.(interface {
		Capabilities(context.Context) app.Capabilities
	})
	return ok && c.Capabilities(ctx).CanBucket
}

// mountBuckets puts a bucket's routes on the router. Members may add one and
// change its files, as they may deploy an app and change its data; deleting
// the bucket is the app's delete, an admin's.
func (s *Server) mountBuckets(r chi.Router) {
	if s.buckets() == nil {
		return
	}
	r.Post("/apps/new/bucket", s.inEnvironment(s.bucketDeploy))
	r.Get("/apps/{name}/files", s.bucketFilesTab)
	r.Get("/apps/{name}/files/list", s.bucketList)
	r.Post("/apps/{name}/files", s.bucketUpload)
	r.Post("/apps/{name}/files/delete", s.bucketDelete)
	r.Get("/apps/{name}/files/download", s.bucketDownload)
}

func (s *Server) bucketDeploy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	project := strings.TrimSpace(r.FormValue("project"))
	projectID, err := s.projectID(ctx, owner.ID, project)
	if err == nil {
		var a app.App
		if a, err = s.buckets().DeployBucket(ctx, owner.ID, projectID); err == nil {
			auditTarget(ctx, a.Name)
			http.Redirect(w, r, "/apps/"+a.Name, http.StatusSeeOther)
			return
		}
	}
	s.log.Warn("add bucket failed", slog.String("error", err.Error()))
	s.renderStatus(w, r, http.StatusUnprocessableEntity, BucketFailed(bucketSentence(err)))
}

// bucketFilesTab is the Files tab, which is also a bucket's panel when no
// tab is named.
func (s *Server) bucketFilesTab(w http.ResponseWriter, r *http.Request) {
	s.renderCanvasWith(w, r, "", chi.URLParam(r, "name"), "files", func(*AppDetailData) {})
}

// BucketListingData is one folder of a bucket, the part of the Files tab
// that changes as somebody moves through it.
type BucketListingData struct {
	App     string
	Bucket  app.Bucket
	Listing objstore.Listing
	// After is where this page of the folder started, for the link back to
	// its first.
	After  string
	Error  string
	Notice string
	// Read says the folder was listed: without it there is nothing to say
	// about what is in it, not even that it is empty.
	Read bool
}

// Crumbs are the folders above this one, each with its prefix.
func (d BucketListingData) Crumbs() [][2]string {
	var out [][2]string
	acc := ""
	for _, part := range strings.Split(strings.TrimSuffix(d.Listing.Prefix, "/"), "/") {
		if part == "" {
			continue
		}
		acc += part + "/"
		out = append(out, [2]string{part, acc})
	}
	return out
}

func (s *Server) bucketList(w http.ResponseWriter, r *http.Request) {
	s.renderBucketListing(w, r, r.URL.Query().Get("prefix"), r.URL.Query().Get("after"), "", "")
}

// renderBucketListing draws one folder, with whatever an upload or a delete
// had to say about itself.
func (s *Server) renderBucketListing(w http.ResponseWriter, r *http.Request, prefix, after, notice, failure string) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	d := BucketListingData{App: name, After: after, Notice: notice, Error: failure}
	if prefix != "" && (objstore.ValidKey(strings.TrimSuffix(prefix, "/")) != nil || !strings.HasSuffix(prefix, "/")) {
		prefix = ""
	}
	d.Listing.Prefix = prefix
	b, err := s.buckets().Bucket(ctx, owner.ID, name)
	if err != nil {
		s.bucketNotFound(w, r, err)
		return
	}
	d.Bucket = b
	files, err := s.buckets().BucketFiles(ctx, owner.ID, name)
	if err == nil {
		var l objstore.Listing
		if l, err = files.List(ctx, prefix, after); err == nil {
			d.Listing, d.Read = l, true
		}
	}
	if err != nil && d.Error == "" {
		d.Error = bucketSentence(err)
	}
	// Bare: every request for a listing is htmx's, swapping it into the
	// Files tab. Inside a Layout it drew the project bar and the rail a
	// second time inside the panel.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := BucketListing(d).Render(ctx, w); err != nil {
		s.log.Error("render bucket listing", slog.String("app", name), slog.String("error", err.Error()))
	}
}

// maxUpload is the most one upload may carry, every file in it together.
// Larger is for a client speaking S3 to the bucket, which streams.
const maxUpload = 1 << 30

func (s *Server) bucketUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	// Files above 32 MiB are spooled to the disk rather than held in
	// memory, and removed when the request is done.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		markAuditFailed(ctx)
		s.renderBucketListing(w, r, r.FormValue("prefix"), "", "",
			"The upload did not arrive whole. Uploads here are up to 1 GiB together; a larger file goes through an S3 client.")
		return
	}
	defer r.MultipartForm.RemoveAll() //nolint:errcheck // temporary files
	prefix := r.FormValue("prefix")
	files, err := s.buckets().BucketFiles(ctx, owner.ID, name)
	if err != nil {
		s.bucketNotFound(w, r, err)
		return
	}
	// Room for it first: a bucket nobody has written to lately keeps only a
	// small part of the store's free room. Failing that, the upload is still
	// tried against the limit it has.
	if err := s.buckets().PrepareBucketUpload(ctx, owner.ID, name); err != nil {
		s.log.Warn("prepare a bucket for an upload", slog.String("app", name), slog.String("error", err.Error()))
	}
	var put []string
	var written int64
	for _, fh := range r.MultipartForm.File["file"] {
		key := prefix + path.Base(strings.ReplaceAll(fh.Filename, `\`, "/"))
		if err := s.putFile(ctx, files, key, fh); err != nil {
			msg := bucketSentence(err)
			if errors.Is(err, objstore.ErrFull) && !s.overPlan(ctx, owner.ID, name, written+fh.Size) {
				msg = installFullSentence
			}
			// The listing answers 200 either way. The files stored before
			// this one were uploaded, and are recorded; none at all is a
			// failure.
			s.auditUploaded(ctx, prefix, put)
			s.renderBucketListing(w, r, prefix, "", uploaded(put), "Could not store "+fh.Filename+": "+msg)
			return
		}
		put = append(put, fh.Filename)
		written += fh.Size
	}
	s.auditUploaded(ctx, prefix, put)
	_ = s.buckets().MeasureBucket(ctx, owner.ID, name)
	s.renderBucketListing(w, r, prefix, "", uploaded(put), "")
}

// auditUploaded records an upload by the names of the files it stored, and
// an upload that stored none as not having happened.
func (s *Server) auditUploaded(ctx context.Context, prefix string, put []string) {
	if len(put) == 0 {
		markAuditFailed(ctx)
		return
	}
	names := put
	if len(names) > 20 {
		names = append(names[:20:20], "…")
	}
	auditDetail(ctx, "files", strings.Join(names, ", "))
	auditDetail(ctx, "count", itoa(len(put)))
	if prefix != "" {
		auditDetail(ctx, "folder", prefix)
	}
}

func (s *Server) putFile(ctx context.Context, files *objstore.Files, key string, fh *multipart.FileHeader) error {
	f, err := fh.Open()
	if err != nil {
		return err
	}
	defer f.Close()
	return files.Put(ctx, key, f, fh.Size, fh.Header.Get("Content-Type"))
}

func uploaded(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return "Uploaded " + names[0] + "."
	}
	return "Uploaded " + itoa(len(names)) + " files."
}

func (s *Server) bucketDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name, key := chi.URLParam(r, "name"), r.FormValue("key")
	prefix := key[:strings.LastIndex(key, "/")+1]
	files, err := s.buckets().BucketFiles(ctx, owner.ID, name)
	if err != nil {
		s.bucketNotFound(w, r, err)
		return
	}
	if err := objstore.ValidKey(key); err != nil {
		markAuditFailed(ctx)
		s.renderBucketListing(w, r, prefix, "", "", err.Error())
		return
	}
	if err := files.Delete(ctx, key); err != nil {
		markAuditFailed(ctx)
		s.renderBucketListing(w, r, prefix, "", "", "Could not delete "+path.Base(key)+": "+bucketSentence(err))
		return
	}
	s.log.Info("bucket file deleted", slog.String("app", name), slog.String("by", s.actorEmail(r)))
	_ = s.buckets().MeasureBucket(ctx, owner.ID, name)
	s.renderBucketListing(w, r, prefix, "", "Deleted "+path.Base(key)+".", "")
}

// bucketDownload sends the browser to the file itself, through a link good
// for that file and a few minutes: the bytes never pass through the
// dashboard.
func (s *Server) bucketDownload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	key := r.URL.Query().Get("key")
	if objstore.ValidKey(key) != nil {
		http.NotFound(w, r)
		return
	}
	files, err := s.buckets().BucketFiles(ctx, owner.ID, chi.URLParam(r, "name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	u, err := files.DownloadURL(key)
	if err != nil {
		http.Error(w, bucketSentence(err), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

func (s *Server) bucketNotFound(w http.ResponseWriter, r *http.Request, err error) {
	markAuditFailed(r.Context())
	if errors.Is(err, app.ErrNotFound) || errors.Is(err, app.ErrNotABucket) {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, BucketListing(BucketListingData{App: chi.URLParam(r, "name"), Error: bucketSentence(err)}))
}

// installFullSentence is a write refused because the install's bucket store
// has no room left, rather than the team's plan: nothing the team can do but
// wait, and nothing about how much anybody else holds.
const installFullSentence = "This server's bucket storage is full. Your files are safe; " +
	"uploads will work again once an operator makes room."

// overPlan says whether a write of size more bytes, refused as full, was
// past the team's plan rather than the install's room. The store refuses
// both alike, so the team's own numbers decide; unreadable, it is the plan,
// which is what the sentence said before there was another.
func (s *Server) overPlan(ctx context.Context, ownerID, name string, size int64) bool {
	b, err := s.buckets().Bucket(ctx, ownerID, name)
	if err != nil {
		return true
	}
	return b.OverPlan(size)
}

// bucketSentence is an error as the page says it.
func bucketSentence(err error) string {
	switch {
	case errors.Is(err, objstore.ErrUnavailable):
		return "The object store is not answering. Files are safe; try again in a minute."
	case errors.Is(err, app.ErrNoBuckets):
		return "This install has no object store for buckets."
	case errors.Is(err, objstore.ErrFull):
		return "The team's buckets hold as much as its plan allows. Delete some files, or move to a plan with more."
	}
	return strings.TrimPrefix(strings.TrimPrefix(err.Error(), "app: "), "bucket: ")
}

// attachBucket loads what a bucket's panel shows of it.
func (s *Server) attachBucket(ctx context.Context, d *AppDetailData) {
	if d.App.Source.Workload() || s.buckets() == nil {
		return
	}
	if b, err := s.buckets().Bucket(ctx, d.App.OwnerID, d.App.Name); err == nil {
		d.Bucket = &b
	} else {
		s.log.Error("read bucket", slog.String("app", d.App.Name), slog.String("error", err.Error()))
	}
}

// bucketTab is the Files tab's slug: none, as Deployments' is for every
// other service, so a bucket's panel opens on its files.
func bucketTab(tab string) string {
	if tab == "files" {
		return ""
	}
	return tab
}

func bucketListHref(app, prefix, after string) string {
	q := url.Values{}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if after != "" {
		q.Set("after", after)
	}
	href := "/apps/" + app + "/files/list"
	if len(q) > 0 {
		href += "?" + q.Encode()
	}
	return href
}

func bucketDownloadHref(app, key string) string {
	return "/apps/" + app + "/files/download?" + url.Values{"key": {key}}.Encode()
}

// folderName is a folder's own name, from its prefix: "2026" from
// "photos/2026/".
func folderName(prefix string) string {
	p := strings.TrimSuffix(prefix, "/")
	return p[strings.LastIndex(p, "/")+1:]
}

// bucketReferences is what an app's variables say to reach the bucket.
func bucketReferences(name string) string {
	var b strings.Builder
	for _, k := range []string{app.BucketEndpoint, app.BucketRegion, app.BucketVar, app.BucketKeyID, app.BucketSecretKey} {
		b.WriteString(k + "=${{" + name + "." + k + "}}\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// bucketRoom says what is left of the team's bucket storage.
func bucketRoom(b app.Bucket) string {
	if left := b.TeamLimit - b.TeamBytes; left > 0 {
		return formatBytes(left) + " left"
	}
	return "full: uploads are refused"
}

// measuredLabel says how fresh a bucket's size is.
func measuredLabel(b app.Bucket) string {
	if b.MeasuredAt.IsZero() {
		return "not measured yet"
	}
	return "measured " + relativeTime(b.MeasuredAt)
}
