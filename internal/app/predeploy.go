package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// ErrNoPreDeployer means a release has a pre-deploy command and this
// install's orchestrator cannot run one.
//
// A failed deploy rather than a skipped command. The command is there because
// the new version needs it — a migration, most often — and starting that
// version without it is a deploy that succeeds and then breaks.
var ErrNoPreDeployer = errors.New(
	"app: this install cannot run a pre-deploy command, so a release with one cannot be deployed")

// PreDeployFailure is a pre-deploy command that ran and did not succeed.
//
// Its own type so the two ways a pre-deploy step can go wrong stay apart: this
// one is the command's answer and ends the deploy, where a cluster that could
// not be asked says nothing about the command and is worth asking again.
type PreDeployFailure struct {
	Result orchestrator.PreDeployResult
}

// preDeployMessageBytes bounds how much of the command's output the failure
// carries. The message is stored on the deployment and shown where it
// failed; the end of the output is where the reason is.
const preDeployMessageBytes = 4000

func (e *PreDeployFailure) Error() string {
	r := e.Result
	var what string
	switch {
	case r.Reason != "" && r.ExitCode != 0:
		what = fmt.Sprintf("exited %d (%s)", r.ExitCode, r.Reason)
	case r.Reason != "":
		what = r.Reason
	default:
		what = fmt.Sprintf("exited %d", r.ExitCode)
	}
	msg := "pre-deploy command " + what
	output := strings.TrimSpace(strings.Join(r.Output, "\n"))
	if output == "" {
		return msg
	}
	if len(output) > preDeployMessageBytes {
		output = "…" + output[len(output)-preDeployMessageBytes:]
	}
	return msg + ":\n" + output
}

// preDeployOperation runs the pre-deploy command of the release an operation
// is about to apply, and returns once it has succeeded.
//
// Run with the operation in its applying stage and before the apply, so the
// new version takes no traffic until the command has finished — and a failure
// leaves the cluster, and the active release, exactly as they were. Outside
// the convergence lock, which serialises cluster writes for the app across
// replicas: a migration can take minutes, and nothing else writes this app's
// workload while its operation is live, so holding the lock would only hold a
// database connection for as long.
func (s *Service) preDeployOperation(ctx context.Context, op Operation) error {
	release, err := s.operationRelease(ctx, op)
	if err != nil {
		return err
	}
	if release.PreDeployCommand == "" {
		return nil
	}
	// A worker displaced since it entered this stage must not run a command
	// the replica that displaced it is about to run too. The lease's own
	// renewal would cancel it within a third of a lease; this closes the
	// window before that.
	if err := s.checkOperationClaim(ctx, op); err != nil {
		return err
	}
	a, err := s.operationApp(ctx, op)
	if err != nil {
		return err
	}
	return s.runPreDeploy(ctx, op, a, release)
}

// recoverPreDeploy is preDeployOperation for an applying stage whose worker
// went away.
//
// The checkpoint cannot say whether the command ran. The workload can say
// whether the apply after it did, and that is the question that matters: a
// release already applied ran its command first, and one not applied is run
// again before it is. Again, rather than assumed done, because a worker that
// stopped mid-command left a command that did not finish — which is why a
// pre-deploy command has to be safe to run twice, as every migration tool's
// is.
//
// A cluster that cannot be read, or cannot run the command, is
// ErrClusterObservation, the stage's own rule for a question the cluster did
// not answer; a command that ran and failed is the deploy's answer.
func (s *Service) recoverPreDeploy(ctx context.Context, op Operation) error {
	release, err := s.operationRelease(ctx, op)
	if err != nil {
		return err
	}
	if release.PreDeployCommand == "" {
		return nil
	}
	a, err := s.operationApp(ctx, op)
	if err != nil {
		return err
	}
	status, observeErr := s.orch.AppStatus(ctx, a.Ref())
	if observeErr != nil && !errors.Is(observeErr, orchestrator.ErrNotFound) {
		return fmt.Errorf("%w: %v", ErrClusterObservation, observeErr)
	}
	if observeErr == nil && convergenceMatches(status, release.ID, a.ConfigVersion) {
		return nil
	}
	if err := s.checkOperationClaim(ctx, op); err != nil {
		return err
	}
	err = s.runPreDeploy(ctx, op, a, release)
	var failed *PreDeployFailure
	if err == nil || errors.As(err, &failed) || errors.Is(err, ErrNoPreDeployer) ||
		ctx.Err() != nil {
		return err
	}
	return fmt.Errorf("%w: %v", ErrClusterObservation, err)
}

// runPreDeploy runs a release's pre-deploy command and reports a command that
// failed as a PreDeployFailure.
func (s *Service) runPreDeploy(ctx context.Context, op Operation, a App, release Release) error {
	runner, ok := s.orch.(orchestrator.PreDeployer)
	if !ok {
		return ErrNoPreDeployer
	}
	spec, err := s.preDeploySpec(ctx, a, release)
	if err != nil {
		return err
	}

	logf := s.preDeployLog(ctx, op)
	logf("\nRunning the pre-deploy command: " + release.PreDeployCommand + "\n")
	result, err := runner.PreDeploy(ctx, orchestrator.PreDeployRequest{
		App: spec, Command: release.PreDeployCommand,
	})
	if err != nil {
		logf("The pre-deploy command could not be run: " + err.Error() + "\n")
		return fmt.Errorf("app: run the pre-deploy command: %w", err)
	}
	for _, line := range result.Output {
		logf(line + "\n")
	}
	if !result.Succeeded {
		failure := &PreDeployFailure{Result: result}
		logf("The pre-deploy command failed, so nothing was deployed. " +
			"The running version is unchanged.\n")
		return failure
	}
	logf("The pre-deploy command finished.\n")
	return nil
}

// preDeploySpec is the workload a release is about to become, as far as the
// pre-deploy command needs it: the release's image, environment, user and
// limits, with the app's current secrets, pull credential and placement.
//
// The same overlays applyRelease reads, for the same reasons — secrets are
// current state, a pool and a region are where the app lives now — but not
// its hostnames, which mean nothing to a command and are reconciled, with
// writes, as part of an apply this is not.
func (s *Service) preDeploySpec(ctx context.Context, a App, release Release) (orchestrator.AppSpec, error) {
	_, secrets, err := s.envFor(ctx, s.q, a.ID)
	if err != nil {
		return orchestrator.AppSpec{}, err
	}
	pool, err := teamPool(ctx, s.q, a.OwnerID)
	if err != nil {
		return orchestrator.AppSpec{}, err
	}
	effective := a
	effective.Source = release.Source
	return release.AppSpec(ReleaseOverlays{
		Ref: a.Ref(), ConfigVersion: a.ConfigVersion,
		RegistryAuth: s.pullAuth(ctx, effective), Secrets: secrets,
		Pool: pool, Region: a.Region,
	}), nil
}

// preDeployLog writes the command's output after the build's, for a deploy
// that built: the build log is the one log a deployment keeps, and the
// command ran on what the build produced. A deploy that built nothing has no
// log to add to, and its output reaches the deployment only through the
// failure message, which carries the end of it.
func (s *Service) preDeployLog(ctx context.Context, op Operation) func(string) {
	build, err := s.q.GetBuildForDeployment(ctx, dbgen.GetBuildForDeploymentParams{
		OwnerID: op.OwnerID, DeploymentID: op.DeploymentID,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.log.Warn("could not find the build log for a pre-deploy command",
				"operation_id", op.ID, "error", err)
		}
		return func(string) {}
	}
	return s.buildLogger(ctx, build.ID)
}

// checkOperationClaim reports whether op still holds its claim at the stage it
// believes it is in.
func (s *Service) checkOperationClaim(ctx context.Context, op Operation) error {
	current, err := s.q.GetDeploymentOperation(ctx,
		dbgen.GetDeploymentOperationParams{OwnerID: op.OwnerID, ID: op.ID})
	if err != nil {
		return err
	}
	if op.ClaimToken == nil || !current.ClaimToken.Valid ||
		uuid.UUID(current.ClaimToken.Bytes) != *op.ClaimToken || current.Status != op.Status {
		return ErrOperationClaimLost
	}
	return nil
}
