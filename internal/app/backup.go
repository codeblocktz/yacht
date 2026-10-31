package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// A database backup is a dump taken by a Job in the database's own namespace,
// with the database's own image — so pg_dump is the server's version — and
// its own password, read from its Secret. Where the dump goes is the install's
// to decide:
//
//   - With a bucket configured (Options.BackupS3), the Job uploads to it
//     through a URL the engine signs for that one object. The bucket's keys
//     never enter the customer's namespace.
//   - Without one, it writes to a second volume beside the database. That
//     survives a dropped table and a bad migration, and not the loss of the
//     cluster, which the Backups tab says.
//
// Scheduled backups are the engine's loop rather than a CronJob. A CronJob's
// Jobs are written once and run for ever, so they would need credentials
// that do not expire — the bucket's own keys, in every customer's namespace —
// and they would run without the engine knowing, so the list would not show
// them. The loop starts each one the way the button does, records it, and
// prunes the old ones; RunBackupScheduler is safe in every engine process at
// once, because the database admits one running job per app.

// Backup kinds, statuses and locations, as stored.
const (
	BackupKindBackup  = "backup"
	BackupKindRestore = "restore"

	BackupRunning   = "running"
	BackupSucceeded = "succeeded"
	BackupFailed    = "failed"

	BackupInS3     = "s3"
	BackupOnVolume = "volume"

	BackupManual    = "manual"
	BackupScheduled = "scheduled"
	// BackupBeforeUpdate is taken because somebody pressed Update on the
	// database: the update deploys once it succeeds. See database_update.go.
	BackupBeforeUpdate = "update"
)

// ErrBackupRunning is a backup or restore asked for while another runs
// against the same database.
var ErrBackupRunning = errors.New(
	"app: a backup or restore of this database is already running — one at a time")

// ErrBackupsUnsupported is a database the engine cannot back up: an engine it
// has no recipe for, or an orchestrator that cannot run a job.
var ErrBackupsUnsupported = errors.New("app: backups are not available for this database")

// backupDeadline bounds a backup or restore job, pulling images included.
const backupDeadline = 2 * time.Hour

// backupURLLifetime is how long a signed URL lasts: past the deadline, so a
// slow dump is not refused at the upload it has been working towards.
const backupURLLifetime = backupDeadline + 30*time.Minute

// backupScratch is where a step hands the dump to the next one, and
// backupMount where the backup volume is mounted.
const (
	backupScratch = "/backup"
	backupMount   = "/backups"
	backupVolume  = "backups"
)

// DefaultBackupToolImage uploads and downloads dumps: curl, as a non-root
// user, and nothing else.
const DefaultBackupToolImage = "curlimages/curl:8.10.1"

// Backup is one backup of a database, or one restore into it.
type Backup struct {
	ID         uuid.UUID
	Kind       string
	Trigger    string
	Status     string
	Location   string
	ObjectKey  string
	BackupID   *uuid.UUID
	SizeBytes  int64
	Error      string
	CreatedBy  string
	StartedAt  time.Time
	FinishedAt *time.Time
}

// BackupOverview is what the Backups tab shows.
type BackupOverview struct {
	// Supported is false, with Because saying why, for a database the engine
	// cannot back up here.
	Supported bool
	Because   string

	// Location is where a new backup goes, and Where the bucket and prefix
	// when that is S3.
	Location string
	Where    string

	Daily   bool
	Keep    int32
	Running bool
	Items   []Backup
}

// backupRecipe is how one engine is dumped and restored. Shell, run by the
// engine's own image, with every value it needs in the environment rather
// than spliced in: $OUT and $IN are paths, and the connection is the
// engine's usual variables.
type backupRecipe struct {
	ext     string
	dump    string
	restore string
	env     func(a App, c DatabaseCredentials) map[string]string
	// password is the variable the recipe reads the password from, filled
	// from the app's own Secret.
	password string
}

var backupRecipes = map[Source]backupRecipe{
	SourcePostgres: {
		ext: "pgdump",
		// The custom format, so a restore can drop and recreate each object
		// itself and in one transaction. Without owners: the role a dump was
		// taken as is not always the one it is restored as.
		dump: `pg_dump --format=custom --no-owner --file="$OUT"`,
		// One transaction: a restore that fails halfway leaves the database
		// as it was rather than half replaced.
		restore: `pg_restore --clean --if-exists --no-owner --no-privileges ` +
			`--exit-on-error --single-transaction --dbname="$PGDATABASE" "$IN"`,
		env: func(a App, c DatabaseCredentials) map[string]string {
			return map[string]string{
				// The Service by its short name, which resolves inside the
				// namespace the job runs in.
				"PGHOST": a.Name, "PGPORT": strconv.Itoa(int(c.Port)),
				"PGUSER": c.User, "PGDATABASE": c.Database,
				"PGAPPNAME": "yacht-backup",
			}
		},
		password: "PGPASSWORD",
	},
}

// backupsFor reports whether a can be backed up here, and why not.
func (s *Service) backupsFor(a App) (backupRecipe, orchestrator.JobRunner, error) {
	if !a.Source.Database() {
		return backupRecipe{}, nil, ErrNotADatabase
	}
	recipe, ok := backupRecipes[a.Source]
	if !ok {
		return backupRecipe{}, nil, fmt.Errorf("%w: backups of %s are not built yet",
			ErrBackupsUnsupported, sourceLabel(a.Source))
	}
	runner, ok := s.orch.(orchestrator.JobRunner)
	if !ok {
		return backupRecipe{}, nil, fmt.Errorf("%w: this cluster cannot run a backup job",
			ErrBackupsUnsupported)
	}
	return recipe, runner, nil
}

func sourceLabel(src Source) string {
	if b, err := BlueprintFor(src); err == nil {
		return b.Label
	}
	return string(src)
}

// Backups is a database's backups and schedule, after settling any that
// have finished since the loop last looked.
func (s *Service) Backups(ctx context.Context, ownerID, name string) (BackupOverview, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return BackupOverview{}, err
	}
	if !a.Source.Database() {
		return BackupOverview{}, ErrNotADatabase
	}
	out := BackupOverview{Supported: true, Location: BackupOnVolume, Keep: 7}
	if s.opts.BackupS3 != nil {
		out.Location, out.Where = BackupInS3, s.opts.BackupS3.Where()
	}
	if _, _, err := s.backupsFor(a); err != nil {
		// The detail after the sentinel's own words, which the tab's heading
		// already says.
		msg := err.Error()
		if i := strings.LastIndex(msg, ": "); i >= 0 {
			msg = msg[i+2:]
		}
		out.Supported, out.Because = false, strings.ToUpper(msg[:1])+msg[1:]
	}

	s.settleBackups(ctx, a.ID)

	sched, err := s.q.GetDatabaseBackupSchedule(ctx, dbgen.GetDatabaseBackupScheduleParams{
		OwnerID: ownerID, AppID: a.ID,
	})
	switch {
	case err == nil:
		out.Daily, out.Keep = sched.Daily, sched.Keep
	case !errors.Is(err, pgx.ErrNoRows):
		return BackupOverview{}, fmt.Errorf("app: read backup schedule: %w", err)
	}

	rows, err := s.q.ListDatabaseBackups(ctx, dbgen.ListDatabaseBackupsParams{
		OwnerID: ownerID, AppID: a.ID, ResultLimit: 100,
	})
	if err != nil {
		return BackupOverview{}, fmt.Errorf("app: list backups: %w", err)
	}
	for _, row := range rows {
		b := toBackup(row)
		out.Running = out.Running || b.Status == BackupRunning
		out.Items = append(out.Items, b)
	}
	return out, nil
}

// StartBackup takes a backup now.
func (s *Service) StartBackup(ctx context.Context, ownerID, name, actor string) (Backup, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return Backup{}, err
	}
	return s.startBackup(ctx, a, BackupManual, actor, nil)
}

// startBackup starts one. then is the update to deploy once it succeeds,
// nil for a backup taken for its own sake.
func (s *Service) startBackup(ctx context.Context, a App, trigger, actor string, then *DatabaseUpdate) (Backup, error) {
	recipe, runner, err := s.backupsFor(a)
	if err != nil {
		return Backup{}, err
	}
	job, image, err := s.backupJobBase(ctx, a, recipe)
	if err != nil {
		return Backup{}, err
	}

	location := BackupOnVolume
	if s.opts.BackupS3 != nil {
		location = BackupInS3
	}
	params := dbgen.CreateDatabaseBackupParams{
		OwnerID: a.OwnerID, AppID: a.ID, Kind: BackupKindBackup, Trigger: trigger,
		Location: location, CreatedBy: actor,
	}
	if then != nil {
		params.UpdateImage, params.UpdateLabel = then.Image, then.Label
	}
	row, err := s.q.CreateDatabaseBackup(ctx, params)
	if err != nil {
		if isUniqueViolation(err) {
			return Backup{}, ErrBackupRunning
		}
		return Backup{}, fmt.Errorf("app: record backup: %w", err)
	}
	b := toBackup(row)

	file := b.StartedAt.UTC().Format("20060102T150405Z") + "-" + b.ID.String()[:8] + "." + recipe.ext
	job.Name = a.Name + "-backup-" + b.ID.String()[:8]

	// The size is the job's one result, left as its termination message.
	switch location {
	case BackupInS3:
		b.ObjectKey = s.opts.BackupS3.Key(a.Namespace + "/" + a.Name + "/" + file)
		put, err := s.opts.BackupS3.Presign("PUT", b.ObjectKey, backupURLLifetime, time.Now())
		if err != nil {
			return s.failBackup(ctx, b, err)
		}
		job.ScratchPath = backupScratch
		job.SecretEnv = map[string]string{"UPLOAD_URL": put}
		job.Env["OUT"] = backupScratch + "/dump"
		job.Steps = []orchestrator.JobStep{
			{Name: "dump", Image: image, Command: shell(recipe.dump)},
			{Name: "upload", Image: s.backupToolImage(), Command: shell(
				`curl --fail --silent --show-error --retry 3 --upload-file "$OUT" ` +
					`--output /dev/null --write-out '%{size_upload}' "$UPLOAD_URL" > /dev/termination-log`)},
		}
	default:
		b.ObjectKey = backupMount + "/" + file
		job.Volume = s.backupVolumeFor(ctx, a)
		job.Env["OUT"] = b.ObjectKey
		// Written beside the final name and moved into place, so a dump cut
		// short is never mistaken for a whole one.
		job.Steps = []orchestrator.JobStep{
			{Name: "dump", Image: image, Command: shell(
				`FINAL="$OUT"; OUT="$OUT.partial"; ` + recipe.dump +
					` && mv "$OUT" "$FINAL" && stat -c %s "$FINAL" > /dev/termination-log`)},
		}
	}

	if err := s.q.SetDatabaseBackupJob(ctx, dbgen.SetDatabaseBackupJobParams{
		ID: b.ID, JobName: job.Name, ObjectKey: b.ObjectKey,
	}); err != nil {
		return s.failBackup(ctx, b, err)
	}
	b.Status = BackupRunning
	if err := runner.RunJob(ctx, job); err != nil {
		return s.failBackup(ctx, b, err)
	}
	s.log.Info("database backup started",
		slog.String("app", a.Name), slog.String("owner", a.OwnerID),
		slog.String("trigger", trigger), slog.String("location", location))
	return b, nil
}

// RestoreBackup replaces a database's contents with a backup's.
func (s *Service) RestoreBackup(ctx context.Context, ownerID, name string, id uuid.UUID, actor string) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	recipe, runner, err := s.backupsFor(a)
	if err != nil {
		return err
	}
	src, err := s.q.GetDatabaseBackup(ctx, dbgen.GetDatabaseBackupParams{OwnerID: ownerID, AppID: a.ID, ID: id})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("app: read backup: %w", err)
	}
	if src.Kind != BackupKindBackup || src.Status != BackupSucceeded {
		return errors.New("app: only a backup that finished can be restored")
	}
	if src.Location == BackupInS3 && s.opts.BackupS3 == nil {
		return errors.New("app: that backup is in a bucket this install is no longer configured with")
	}
	job, image, err := s.backupJobBase(ctx, a, recipe)
	if err != nil {
		return err
	}

	row, err := s.q.CreateDatabaseBackup(ctx, dbgen.CreateDatabaseBackupParams{
		OwnerID: ownerID, AppID: a.ID, Kind: BackupKindRestore, Trigger: BackupManual,
		Location: src.Location, ObjectKey: src.ObjectKey,
		BackupID: pgtype.UUID{Bytes: src.ID, Valid: true}, CreatedBy: actor,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return ErrBackupRunning
		}
		return fmt.Errorf("app: record restore: %w", err)
	}
	r := toBackup(row)
	job.Name = a.Name + "-restore-" + r.ID.String()[:8]

	switch src.Location {
	case BackupInS3:
		get, err := s.opts.BackupS3.Presign("GET", src.ObjectKey, backupURLLifetime, time.Now())
		if err != nil {
			_, err = s.failBackup(ctx, r, err)
			return err
		}
		job.ScratchPath = backupScratch
		job.SecretEnv = map[string]string{"DOWNLOAD_URL": get}
		job.Env["IN"] = backupScratch + "/dump"
		job.Steps = []orchestrator.JobStep{
			{Name: "download", Image: s.backupToolImage(), Command: shell(
				`curl --fail --silent --show-error --retry 3 --output "$IN" "$DOWNLOAD_URL"`)},
			{Name: "restore", Image: image, Command: shell(recipe.restore)},
		}
	default:
		job.Volume = s.backupVolumeFor(ctx, a)
		job.Env["IN"] = src.ObjectKey
		job.Steps = []orchestrator.JobStep{
			{Name: "restore", Image: image, Command: shell(recipe.restore)},
		}
	}

	if err := s.q.SetDatabaseBackupJob(ctx, dbgen.SetDatabaseBackupJobParams{
		ID: r.ID, JobName: job.Name, ObjectKey: src.ObjectKey,
	}); err != nil {
		_, err = s.failBackup(ctx, r, err)
		return err
	}
	if err := runner.RunJob(ctx, job); err != nil {
		_, err = s.failBackup(ctx, r, err)
		return err
	}
	s.log.Info("database restore started",
		slog.String("app", a.Name), slog.String("owner", a.OwnerID),
		slog.String("backup", src.ID.String()))
	return nil
}

// DeleteBackup removes a backup and its dump.
//
// The dump first, the record after: a record whose dump is gone is a backup
// somebody would try to restore, while a dump whose record is gone is only
// storage.
func (s *Service) DeleteBackup(ctx context.Context, ownerID, name string, id uuid.UUID) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	row, err := s.q.GetDatabaseBackup(ctx, dbgen.GetDatabaseBackupParams{OwnerID: ownerID, AppID: a.ID, ID: id})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("app: read backup: %w", err)
	}
	if row.Status == BackupRunning {
		return ErrBackupRunning
	}
	// Nor the dump a restore is reading right now.
	recent, err := s.q.ListDatabaseBackups(ctx, dbgen.ListDatabaseBackupsParams{
		OwnerID: ownerID, AppID: a.ID, ResultLimit: 20,
	})
	if err != nil {
		return fmt.Errorf("app: list backups: %w", err)
	}
	for _, r := range recent {
		if r.Status == BackupRunning && r.BackupID.Valid && uuid.UUID(r.BackupID.Bytes) == id {
			return ErrBackupRunning
		}
	}
	if err := s.removeDump(ctx, a, row); err != nil {
		return err
	}
	if _, err := s.q.DeleteDatabaseBackup(ctx, dbgen.DeleteDatabaseBackupParams{
		OwnerID: ownerID, AppID: a.ID, ID: id,
	}); err != nil {
		return fmt.Errorf("app: delete backup: %w", err)
	}
	return nil
}

// removeDump deletes what a backup row points at. A restore's row points at
// its source's dump, which is not its to delete.
func (s *Service) removeDump(ctx context.Context, a App, row dbgen.DatabaseBackup) error {
	if row.Kind != BackupKindBackup || row.ObjectKey == "" {
		return nil
	}
	switch row.Location {
	case BackupInS3:
		if s.opts.BackupS3 == nil {
			return errors.New("app: that backup is in a bucket this install is no longer configured with")
		}
		return s.opts.BackupS3.Delete(ctx, row.ObjectKey)
	default:
		// The file is on a claim only a pod can reach, so a pod removes it.
		// Not waited for: the dump is unreachable once its record is gone,
		// and a removal that fails leaves storage behind, not a backup.
		recipe, runner, err := s.backupsFor(a)
		if err != nil {
			return err
		}
		job, image, err := s.backupJobBase(ctx, a, recipe)
		if err != nil {
			return err
		}
		job.Name = a.Name + "-rmbackup-" + row.ID.String()[:8]
		job.Volume = s.backupVolumeFor(ctx, a)
		job.Env = map[string]string{"IN": row.ObjectKey}
		job.AppSecretEnv = nil
		job.Steps = []orchestrator.JobStep{{Name: "remove", Image: image,
			Command: shell(`rm -f "$IN" "$IN.partial"`)}}
		return runner.RunJob(ctx, job)
	}
}

// SetBackupSchedule turns the daily backup on or off.
func (s *Service) SetBackupSchedule(ctx context.Context, ownerID, name string, daily bool) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if daily {
		if _, _, err := s.backupsFor(a); err != nil {
			return err
		}
	}
	if err := s.q.SetDatabaseBackupSchedule(ctx, dbgen.SetDatabaseBackupScheduleParams{
		AppID: a.ID, OwnerID: ownerID, Daily: daily,
	}); err != nil {
		return fmt.Errorf("app: set backup schedule: %w", err)
	}
	return nil
}

// backupJobBase is what every backup job of an app shares: where it runs, as
// whom, with which image and which connection. The image is the one the
// database runs now, pinned, so the dump tool is the server's own version.
func (s *Service) backupJobBase(ctx context.Context, a App, recipe backupRecipe) (orchestrator.JobSpec, string, error) {
	if a.ActiveReleaseID == nil {
		return orchestrator.JobSpec{}, "", fmt.Errorf("app: %s has not been deployed yet, so there is nothing to back up", a.Name)
	}
	rel, err := s.ReleaseByID(ctx, a.OwnerID, a.ID, *a.ActiveReleaseID)
	if err != nil {
		return orchestrator.JobSpec{}, "", err
	}
	creds, err := s.databaseCredentials(ctx, a)
	if err != nil {
		return orchestrator.JobSpec{}, "", err
	}
	keys := databaseKeysBySource[a.Source]
	pool, err := teamPool(ctx, s.q, a.OwnerID)
	if err != nil {
		return orchestrator.JobSpec{}, "", err
	}
	uid := runtimeOf(a).RunAsUser
	return orchestrator.JobSpec{
		Ref:          a.Ref(),
		RunAsUser:    uid,
		Env:          recipe.env(a, creds),
		AppSecretEnv: map[string]string{recipe.password: keys.password},
		Deadline:     backupDeadline,
		Pool:         pool,
		Region:       a.Region,
	}, pinImage(rel.ImageRef, rel.ImageDigest), nil
}

// backupVolumeFor is the claim backups are kept on when there is no bucket:
// as large as the database's own, on the same storage class.
func (s *Service) backupVolumeFor(ctx context.Context, a App) *orchestrator.VolumeSpec {
	v := &orchestrator.VolumeSpec{Name: backupVolume, MountPath: backupMount, SizeBytes: 1 << 30}
	if vols, err := s.volumesFor(ctx, s.q, a.ID); err == nil && len(vols) > 0 {
		v.SizeBytes, v.Class = vols[0].SizeBytes, vols[0].Class
	}
	return v
}

func (s *Service) backupToolImage() string {
	if s.opts.BackupToolImage != "" {
		return s.opts.BackupToolImage
	}
	return DefaultBackupToolImage
}

// shell runs a script with sh, failing at the first command that does.
func shell(script string) []string { return []string{"sh", "-ec", script} }

func (s *Service) failBackup(ctx context.Context, b Backup, cause error) (Backup, error) {
	if _, err := s.q.FinishDatabaseBackup(ctx, dbgen.FinishDatabaseBackupParams{
		ID: b.ID, Status: BackupFailed, Error: cause.Error(),
	}); err != nil {
		s.log.Error("record a backup that could not start", slog.String("error", err.Error()))
	}
	return Backup{}, cause
}

// settleBackups records the outcome of every running job that has finished:
// one app's when appID is set, every app's otherwise.
func (s *Service) settleBackups(ctx context.Context, appID uuid.UUID) {
	runner, ok := s.orch.(orchestrator.JobRunner)
	if !ok {
		return
	}
	rows, err := s.q.ListRunningDatabaseBackups(ctx)
	if err != nil {
		s.log.Error("list running backups", slog.String("error", err.Error()))
		return
	}
	for _, row := range rows {
		if appID != uuid.Nil && row.AppID != appID {
			continue
		}
		age := time.Since(row.StartedAt)
		var status, reason string
		var size int64
		switch st, err := runner.JobState(ctx, row.AppNamespace, row.JobName); {
		case err != nil:
			s.log.Warn("read a backup job", slog.String("job", row.JobName), slog.String("error", err.Error()))
			continue
		case !st.Found:
			// A Job is visible as soon as it is created, so one missing for a
			// minute was never made or has been removed — by hand, or with
			// its namespace.
			if age < time.Minute {
				continue
			}
			status, reason = BackupFailed, "the job running it is gone from the cluster"
		case st.Done && st.Failed:
			status, reason = BackupFailed, st.Reason
		case st.Done:
			status = BackupSucceeded
			size, _ = strconv.ParseInt(strings.TrimSpace(st.Message), 10, 64)
		case age > backupDeadline+10*time.Minute:
			status, reason = BackupFailed, "it ran past its deadline"
		default:
			continue
		}
		n, err := s.q.FinishDatabaseBackup(ctx, dbgen.FinishDatabaseBackupParams{
			ID: row.ID, Status: status, SizeBytes: size, Error: reason,
		})
		if err != nil {
			s.log.Error("record a backup's outcome", slog.String("error", err.Error()))
			continue
		}
		if n == 0 {
			// Another engine process settled it first.
			continue
		}
		if row.JobName != "" {
			if err := runner.DeleteJob(ctx, row.AppNamespace, row.JobName); err != nil {
				s.log.Warn("clean up a backup job", slog.String("job", row.JobName), slog.String("error", err.Error()))
			}
		}
		s.log.Info("database "+row.Kind+" "+status,
			slog.String("app", row.AppName), slog.String("owner", row.OwnerID),
			slog.String("reason", reason))
		if status == BackupSucceeded && row.Kind == BackupKindBackup && row.Trigger == BackupScheduled {
			s.pruneScheduled(ctx, row.OwnerID, row.AppName)
		}
		// Only the process whose FinishDatabaseBackup changed the row is
		// here, so an update waiting on a backup deploys once.
		if row.Trigger == BackupBeforeUpdate && row.UpdateImage != "" {
			s.afterUpdateBackup(ctx, row.OwnerID, row.AppName, DatabaseUpdate{
				Image: row.UpdateImage, Label: row.UpdateLabel,
			}, status, reason)
		}
	}
}

// pruneScheduled deletes the scheduled backups past the number kept.
func (s *Service) pruneScheduled(ctx context.Context, ownerID, name string) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return
	}
	keep := int32(7)
	if sched, err := s.q.GetDatabaseBackupSchedule(ctx, dbgen.GetDatabaseBackupScheduleParams{
		OwnerID: ownerID, AppID: a.ID,
	}); err == nil {
		keep = sched.Keep
	}
	old, err := s.q.ListExpiredScheduledBackups(ctx, dbgen.ListExpiredScheduledBackupsParams{
		OwnerID: ownerID, AppID: a.ID, Keep: keep,
	})
	if err != nil {
		s.log.Error("list expired backups", slog.String("error", err.Error()))
		return
	}
	for _, row := range old {
		if err := s.DeleteBackup(ctx, ownerID, name, row.ID); err != nil {
			s.log.Warn("prune a scheduled backup", slog.String("app", name), slog.String("error", err.Error()))
		}
	}
}

// RunBackupScheduler settles finished backup jobs and starts the daily ones
// that are due, until ctx ends.
func (s *Service) RunBackupScheduler(ctx context.Context) {
	if _, ok := s.orch.(orchestrator.JobRunner); !ok {
		return
	}
	interval := s.opts.BackupCheckInterval
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.settleBackups(ctx, uuid.Nil)
		s.startDueBackups(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) startDueBackups(ctx context.Context) {
	due, err := s.q.ListDueDatabaseBackups(ctx)
	if err != nil {
		s.log.Error("list due backups", slog.String("error", err.Error()))
		return
	}
	for _, d := range due {
		a, err := s.Get(ctx, d.OwnerID, d.Name)
		if err != nil {
			continue
		}
		if _, err := s.startBackup(ctx, a, BackupScheduled, "", nil); err != nil && !errors.Is(err, ErrBackupRunning) {
			s.log.Warn("start a scheduled backup", slog.String("app", d.Name), slog.String("error", err.Error()))
		}
	}
}

func toBackup(row dbgen.DatabaseBackup) Backup {
	b := Backup{
		ID: row.ID, Kind: row.Kind, Trigger: row.Trigger, Status: row.Status,
		Location: row.Location, ObjectKey: row.ObjectKey, SizeBytes: row.SizeBytes,
		Error: row.Error, CreatedBy: row.CreatedBy, StartedAt: row.StartedAt,
	}
	if row.BackupID.Valid {
		id := uuid.UUID(row.BackupID.Bytes)
		b.BackupID = &id
	}
	if row.FinishedAt.Valid {
		t := row.FinishedAt.Time
		b.FinishedAt = &t
	}
	return b
}
