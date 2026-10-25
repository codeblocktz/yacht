package orchestrator

import (
	"context"
	"errors"
	"time"
)

// JobRunner runs one-off work beside a workload, in its namespace and with
// its secrets: a database's backup, or a restore into it.
//
// Optional, like Builder. An orchestrator without it has no way to run a
// program to completion next to an app, and the dashboard says backups are
// not available on it rather than offering a button that fails.
type JobRunner interface {
	// RunJob starts a job. A job whose name is already taken is left alone
	// and reported as started: the caller names jobs after the record they
	// settle, so a second start is a retry of the first.
	RunJob(ctx context.Context, spec JobSpec) error

	// JobState reports what became of a job. Found false means the cluster
	// has no such job — never started, or cleaned up since.
	JobState(ctx context.Context, namespace, name string) (JobState, error)

	// DeleteJob removes a job and what it made. An absent job is success.
	DeleteJob(ctx context.Context, namespace, name string) error
}

// JobSpec describes one job.
type JobSpec struct {
	// Ref is the app the job works for. The job runs in its namespace, under
	// its owner's labels, in its pool and region.
	Ref  Ref
	Name string

	// Steps run one after another, each in a container of its own image; a
	// step that fails ends the job. Several steps rather than one script,
	// because the work spans images: pg_dump is in the database's image and
	// an upload client is not.
	Steps []JobStep

	// RunAsUser is the uid and gid every step runs as, and the group that
	// owns the shared directory and the volume.
	RunAsUser int64

	// Env is given to every step. SecretEnv is too, from a Secret made for
	// this job alone and deleted with it — for what must not be read off the
	// job's own definition, such as a signed URL.
	Env       map[string]string
	SecretEnv map[string]string

	// AppSecretEnv gives every step values from the app's own secret
	// environment: the variable's name in the step, and its key in the app's.
	AppSecretEnv map[string]string

	// ScratchPath is an empty directory every step shares, for handing a
	// file from one step to the next.
	ScratchPath string

	// Volume, when set, is one of the app's claims mounted into every step,
	// created first if it does not exist.
	Volume *VolumeSpec

	// Deadline bounds the whole job, pulling images included.
	Deadline time.Duration

	Pool   string
	Region string
}

// JobStep is one container of a job.
type JobStep struct {
	Name    string
	Image   string
	Command []string
}

// Validate checks the spec before anything is sent to a cluster.
func (s JobSpec) Validate() error {
	if err := s.Ref.Validate(); err != nil {
		return err
	}
	if err := ValidateDNSLabel("job name", s.Name); err != nil {
		return err
	}
	if len(s.Steps) == 0 {
		return errors.New("job: at least one step is required")
	}
	for _, st := range s.Steps {
		if err := ValidateDNSLabel("job step", st.Name); err != nil {
			return err
		}
		if st.Image == "" || len(st.Command) == 0 {
			return errors.New("job: every step needs an image and a command")
		}
	}
	if s.RunAsUser <= 0 {
		return errors.New("job: a job runs as a non-root user it names")
	}
	return nil
}

// JobState is what the cluster says about a job.
type JobState struct {
	Found  bool
	Done   bool
	Failed bool

	// Reason is why it failed, from the step that did. Message is what the
	// last step left in its termination message when it succeeded: a job's
	// one way to hand a result back, such as the size of what it wrote.
	Reason  string
	Message string
}
