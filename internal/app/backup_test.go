package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/codeblocktz/yacht/internal/backup"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// jobOrchestrator runs jobs by remembering them, and finishes them when a
// test says so.
type jobOrchestrator struct {
	*addressedOrchestrator
	mu      sync.Mutex
	jobs    map[string]orchestrator.JobSpec
	states  map[string]orchestrator.JobState
	deleted []string
}

func (o *jobOrchestrator) RunJob(_ context.Context, spec orchestrator.JobSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.jobs[spec.Name] = spec
	o.states[spec.Name] = orchestrator.JobState{Found: true}
	return nil
}

func (o *jobOrchestrator) JobState(_ context.Context, _, name string) (orchestrator.JobState, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.states[name], nil
}

func (o *jobOrchestrator) DeleteJob(_ context.Context, _, name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.deleted = append(o.deleted, name)
	delete(o.states, name)
	return nil
}

func (o *jobOrchestrator) finish(name string, st orchestrator.JobState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	st.Found, st.Done = true, true
	o.states[name] = st
}

func (o *jobOrchestrator) only(t *testing.T, prefix string) orchestrator.JobSpec {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	var found []orchestrator.JobSpec
	for name, spec := range o.jobs {
		if strings.Contains(name, prefix) {
			found = append(found, spec)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d jobs named like %q, want one", len(found), prefix)
	}
	return found[0]
}

func backupService(t *testing.T, opts func(*Options)) (*Service, *jobOrchestrator, string) {
	t.Helper()
	s, addressed, id := databaseService(t, "")
	orch := &jobOrchestrator{
		addressedOrchestrator: addressed,
		jobs:                  map[string]orchestrator.JobSpec{},
		states:                map[string]orchestrator.JobState{},
	}
	o := s.opts
	if opts != nil {
		opts(&o)
	}
	return NewService(s.pool, orch, s.log, o), orch, id
}

func TestABackupWithoutABucketGoesToAVolumeBesideTheDatabase(t *testing.T) {
	s, orch, id := backupService(t, nil)
	ctx := context.Background()
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{Source: SourcePostgres, Name: "pg"}); err != nil {
		t.Fatal(err)
	}

	b, err := s.StartBackup(ctx, id, "pg", "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	job := orch.only(t, "-backup-")
	if job.Ref.Name != "pg" || job.RunAsUser != 70 || len(job.Steps) != 1 {
		t.Fatalf("job = %+v", job)
	}
	if !strings.Contains(job.Steps[0].Image, "@sha256:") {
		t.Errorf("image %q is not the database's pinned image", job.Steps[0].Image)
	}
	script := strings.Join(job.Steps[0].Command, " ")
	if !strings.Contains(script, "pg_dump --format=custom") || !strings.Contains(script, "mv ") {
		t.Errorf("dump script = %q", script)
	}
	if job.Volume == nil || job.Volume.Name != "backups" || job.Volume.SizeBytes != 1<<30 {
		t.Errorf("volume = %+v", job.Volume)
	}
	if job.AppSecretEnv["PGPASSWORD"] != "POSTGRES_PASSWORD" || job.Env["PGHOST"] != "pg" ||
		job.Env["PGUSER"] != "yacht" || job.Env["PGDATABASE"] != "yacht" {
		t.Errorf("connection: env=%v secrets=%v", job.Env, job.AppSecretEnv)
	}
	if len(job.SecretEnv) != 0 {
		t.Errorf("a volume backup carries secrets of its own: %v", job.SecretEnv)
	}

	// One at a time, whichever kind.
	if _, err := s.StartBackup(ctx, id, "pg", ""); !errors.Is(err, ErrBackupRunning) {
		t.Errorf("a second backup while one runs: %v", err)
	}

	orch.finish(job.Name, orchestrator.JobState{Message: "4096"})
	over, err := s.Backups(ctx, id, "pg")
	if err != nil {
		t.Fatal(err)
	}
	if !over.Supported || over.Location != BackupOnVolume || over.Running || len(over.Items) != 1 {
		t.Fatalf("overview = %+v", over)
	}
	got := over.Items[0]
	if got.ID != b.ID || got.Status != BackupSucceeded || got.SizeBytes != 4096 ||
		got.CreatedBy != "ana@example.com" || got.FinishedAt == nil ||
		!strings.HasPrefix(got.ObjectKey, "/backups/") {
		t.Errorf("backup = %+v", got)
	}
	if len(orch.deleted) != 1 {
		t.Errorf("the finished job was not cleaned up: %v", orch.deleted)
	}

	// Restoring reads the same file, with the same password.
	if err := s.RestoreBackup(ctx, id, "pg", got.ID, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	restore := orch.only(t, "-restore-")
	if restore.Env["IN"] != got.ObjectKey || !strings.Contains(strings.Join(restore.Steps[0].Command, " "),
		"pg_restore --clean --if-exists") {
		t.Errorf("restore = %+v", restore)
	}
	if err := s.DeleteBackup(ctx, id, "pg", got.ID); !errors.Is(err, ErrBackupRunning) {
		t.Errorf("deleting the dump a restore is reading: %v", err)
	}
	orch.finish(restore.Name, orchestrator.JobState{Failed: true, Reason: "the restore step exited 1: boom"})
	over, _ = s.Backups(ctx, id, "pg")
	if over.Items[0].Kind != BackupKindRestore || over.Items[0].Status != BackupFailed ||
		!strings.Contains(over.Items[0].Error, "boom") || over.Items[0].BackupID == nil {
		t.Errorf("restore record = %+v", over.Items[0])
	}

	// Deleting a volume backup removes the file with a job of its own.
	if err := s.DeleteBackup(ctx, id, "pg", got.ID); err != nil {
		t.Fatal(err)
	}
	rm := orch.only(t, "-rmbackup-")
	if rm.Env["IN"] != got.ObjectKey || len(rm.AppSecretEnv) != 0 {
		t.Errorf("removal = %+v", rm)
	}
	over, _ = s.Backups(ctx, id, "pg")
	if len(over.Items) != 1 || over.Items[0].BackupID != nil {
		t.Errorf("after delete: %+v — the restore's record stays, without its source", over.Items)
	}
}

func TestABackupToABucketUploadsThroughASignedURL(t *testing.T) {
	var deletes []string
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes = append(deletes, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer store.Close()
	bucket, err := backup.NewS3(store.URL, "auto", "backups", "databases", "AKIDEXAMPLE", "SECRETEXAMPLE")
	if err != nil {
		t.Fatal(err)
	}
	s, orch, id := backupService(t, func(o *Options) { o.BackupS3 = bucket; o.BackupToolImage = "curl:test" })
	ctx := context.Background()
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{Source: SourcePostgres, Name: "pg"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBackup(ctx, id, "pg", ""); err != nil {
		t.Fatal(err)
	}
	job := orch.only(t, "-backup-")
	if len(job.Steps) != 2 || job.Steps[0].Name != "dump" || job.Steps[1].Name != "upload" ||
		job.Steps[1].Image != "curl:test" || job.ScratchPath == "" || job.Volume != nil {
		t.Fatalf("job = %+v", job)
	}
	put := job.SecretEnv["UPLOAD_URL"]
	if !strings.Contains(put, "/backups/databases/") || !strings.Contains(put, "X-Amz-Signature=") {
		t.Errorf("upload URL = %q", put)
	}
	// What reaches the customer's namespace is one signed URL; the keys do not.
	for _, v := range []string{put, strings.Join(job.Steps[1].Command, " ")} {
		if strings.Contains(v, "SECRETEXAMPLE") {
			t.Fatal("the bucket's secret key is in the job")
		}
	}
	for k, v := range job.Env {
		if strings.Contains(v, "SECRETEXAMPLE") || strings.Contains(v, "X-Amz-Signature") {
			t.Errorf("%s carries a credential in the job's plain environment", k)
		}
	}

	orch.finish(job.Name, orchestrator.JobState{Message: "1234\n"})
	over, err := s.Backups(ctx, id, "pg")
	if err != nil {
		t.Fatal(err)
	}
	if over.Location != BackupInS3 || !strings.HasSuffix(over.Where, "/backups/databases") {
		t.Errorf("overview says %s at %q", over.Location, over.Where)
	}
	b := over.Items[0]
	if b.Status != BackupSucceeded || b.SizeBytes != 1234 ||
		!strings.HasPrefix(b.ObjectKey, "databases/yacht-") {
		t.Fatalf("backup = %+v", b)
	}

	if err := s.RestoreBackup(ctx, id, "pg", b.ID, ""); err != nil {
		t.Fatal(err)
	}
	restore := orch.only(t, "-restore-")
	if restore.Steps[0].Name != "download" || !strings.Contains(restore.SecretEnv["DOWNLOAD_URL"], b.ObjectKey[len("databases/"):]) {
		t.Errorf("restore = %+v", restore)
	}
	orch.finish(restore.Name, orchestrator.JobState{})
	_, _ = s.Backups(ctx, id, "pg")

	if err := s.DeleteBackup(ctx, id, "pg", b.ID); err != nil {
		t.Fatal(err)
	}
	if len(deletes) != 1 || !strings.HasSuffix(deletes[0], b.ObjectKey[len("databases/"):]) {
		t.Errorf("deletes = %v", deletes)
	}
}

func TestTheDailyScheduleStartsOneAndKeepsTheLastFew(t *testing.T) {
	s, orch, id := backupService(t, nil)
	ctx := context.Background()
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{Source: SourcePostgres, Name: "pg"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBackupSchedule(ctx, id, "pg", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE database_backup_schedules SET keep = 2`); err != nil {
		t.Fatal(err)
	}

	// Three nights: each night's backup is started by the loop and finished.
	for night := 0; night < 3; night++ {
		s.startDueBackups(ctx)
		s.startDueBackups(ctx) // a second engine process, the same minute
		over, _ := s.Backups(ctx, id, "pg")
		running := 0
		for _, b := range over.Items {
			if b.Status == BackupRunning {
				running++
				if b.Trigger != BackupScheduled {
					t.Errorf("trigger = %q", b.Trigger)
				}
			}
		}
		if running != 1 {
			t.Fatalf("night %d: %d running, want exactly one", night, running)
		}
		for name := range orch.jobs {
			if st := orch.states[name]; st.Found && !st.Done {
				orch.finish(name, orchestrator.JobState{Message: "10"})
			}
		}
		s.settleBackups(ctx, [16]byte{})
		// Not due again until tomorrow.
		s.startDueBackups(ctx)
		if over, _ := s.Backups(ctx, id, "pg"); over.Running {
			t.Fatalf("night %d: started again the same day", night)
		}
		// Tomorrow.
		if _, err := s.pool.Exec(ctx,
			`UPDATE database_backups SET started_at = started_at - interval '1 day'`); err != nil {
			t.Fatal(err)
		}
	}
	over, _ := s.Backups(ctx, id, "pg")
	if len(over.Items) != 2 || !over.Daily {
		t.Errorf("kept %d scheduled backups, daily=%v; want the last two", len(over.Items), over.Daily)
	}

	if err := s.SetBackupSchedule(ctx, id, "pg", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE database_backups SET started_at = started_at - interval '1 day'`); err != nil {
		t.Fatal(err)
	}
	s.startDueBackups(ctx)
	if over, _ := s.Backups(ctx, id, "pg"); over.Running {
		t.Error("a backup started with the schedule off")
	}
}

func TestEnginesWithoutARecipeSayBackupsAreNotBuilt(t *testing.T) {
	s, _, id := backupService(t, nil)
	ctx := context.Background()
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{Source: SourceRedis, Name: "cache"}); err != nil {
		t.Fatal(err)
	}
	over, err := s.Backups(ctx, id, "cache")
	if err != nil {
		t.Fatal(err)
	}
	if over.Supported || !strings.Contains(over.Because, "Redis") {
		t.Errorf("overview = %+v", over)
	}
	if _, err := s.StartBackup(ctx, id, "cache", ""); !errors.Is(err, ErrBackupsUnsupported) {
		t.Errorf("StartBackup = %v", err)
	}
	if err := s.SetBackupSchedule(ctx, id, "cache", true); !errors.Is(err, ErrBackupsUnsupported) {
		t.Errorf("a schedule for what cannot be backed up: %v", err)
	}
}
