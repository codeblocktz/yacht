package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/domain"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// buildVariables is what a build is given: the app's variables as the deploy
// it is part of will set them, and the values among them the build log must
// not show.
//
// Staged edits are included. The release made after the build carries them
// (createRelease commits them), so a build without them would be an image
// built for different variables than it runs with. References are resolved
// and secrets opened the way envFor does at apply, and the platform's own
// variables are added the way addPlatformVariables does, an app's own
// winning: a Dockerfile that declares ARG DATAHURU_PUBLIC_DOMAIN is told the
// domain the app will be served on.
//
// Secrets and resolved references are both redacted from the log, as both
// reach the cluster as secrets at deploy.
func (s *Service) buildVariables(
	ctx context.Context, ownerID string, a App,
) (vars map[string]string, redact []string, err error) {
	rows, err := s.stagedVariablesFor(ctx, s.q, ownerID, a.ID)
	if err != nil {
		return nil, nil, err
	}
	vars = make(map[string]string, len(rows))
	for _, v := range rows {
		if !v.desired() {
			continue
		}
		value := v.value()
		switch {
		case v.secret():
			var sealed []byte
			if v.Staged != nil {
				sealed = v.Staged.Sealed
			} else {
				sealed = v.Applied.Sealed
			}
			if value, err = s.keeper.Open(sealed); err != nil {
				// The key, never the value, as in envFor.
				return nil, nil, fmt.Errorf("app: opening %s for the build: %w", v.Key, err)
			}
			redact = append(redact, value)
		case HasReference(value):
			if value, err = s.resolveReferences(ctx, s.q, ownerID, scopeOf(a), v.Key, value, 0); err != nil {
				return nil, nil, err
			}
			redact = append(redact, value)
		}
		vars[v.Key] = value
	}

	// The hostnames the apply would hand the ingress, read rather than
	// reconciled: a build changes nothing about routing.
	var hosts []string
	if !a.Internal && a.Port != 0 {
		if hosts, err = domain.RoutableHosts(ctx, s.q, a.ID); err != nil {
			return nil, nil, fmt.Errorf("app: read hostnames for the build: %w", err)
		}
	}
	for k, v := range s.platformEnv(ctx, s.q, a, hosts) {
		if _, own := vars[k]; !own {
			vars[k] = v
		}
	}
	return vars, redact, nil
}

// recordBuildArgs keeps what a build said its Dockerfile declares, for
// variablesNeedBuild. Logged rather than returned, like the image's user: the
// build worked, and not knowing only means the next variable change builds.
func (s *Service) recordBuildArgs(ctx context.Context, buildID uuid.UUID, result orchestrator.BuildResult) {
	if result.BuildArgs == nil {
		return
	}
	if err := s.q.SetBuildArgs(ctx, dbgen.SetBuildArgsParams{
		ID: buildID, BuildArgs: result.BuildArgs,
	}); err != nil {
		s.log.Warn("record the build's arguments", "error", err)
	}
}

// variablesNeedBuild reports whether deploying these changes to a Git app
// needs a new build, rather than the image it already has.
//
// A variable reaches the image only through a build: a Dockerfile's ARG, or
// a buildpacks build's environment. So a change to one the last build was
// given means that image was built with the old value, and Railway's answer
// — build again — is the only one that deploys what somebody typed. A change
// to any other variable deploys on the image as it is, which is minutes
// faster and changes nothing in it.
//
// The names are the ones the image's build recorded. The Dockerfile can only
// change with a commit, and a commit builds, so they are current. A build
// from before builds were given variables recorded none, and is assumed to
// need one.
//
// An image built from an upload cannot be built again: the archive goes
// once its build is over, and building the repository, where there is one,
// would ship different code under the same change. A declared ARG that
// changed is refused with an UploadRebuildError rather than deployed on the
// old image, which would say it succeeded and ship the old value. Only a
// declared one: a build that did not say, or a buildpacks build that was
// given everything, would refuse every variable an uploaded app ever has.
func (s *Service) variablesNeedBuild(ctx context.Context, a App, changes []StagedChange) (bool, error) {
	if a.Source != SourceGit || a.Image == PendingImage {
		return false, nil
	}
	var keys []string
	for _, c := range changes {
		if !c.Setting {
			keys = append(keys, c.Key)
		}
	}
	if len(keys) == 0 {
		return false, nil
	}
	build, err := s.q.GetSuccessfulBuildForImage(ctx, dbgen.GetSuccessfulBuildForImageParams{
		OwnerID: a.OwnerID, AppID: a.ID, Image: a.Image,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("app: read the image's build: %w", err)
	}
	if strings.HasPrefix(build.RepoUrl, UploadTriggerPrefix) {
		var declared []string
		for _, k := range keys {
			if slices.Contains(build.BuildArgs, k) {
				declared = append(declared, k)
			}
		}
		if len(declared) > 0 {
			return false, UploadRebuildError{Keys: declared}
		}
		return false, nil
	}
	if !a.Repo.Set() {
		return false, nil
	}
	return argsUseAny(build.BuildArgs, keys), nil
}

// UploadRebuildError is a staged change to a variable the running image's
// Dockerfile declared as an ARG, when that image was built from source
// uploaded with up: only another up can build it with the new value. The
// change stays staged, and the next up's build carries it.
type UploadRebuildError struct{ Keys []string }

func (e UploadRebuildError) Error() string { return "app: " + e.Explain("") }

// Explain is the refusal in words, naming the CLI the way its users call it;
// the service does not know its name, and says up alone.
func (e UploadRebuildError) Explain(cli string) string {
	up := "up"
	if cli != "" {
		up = cli + " up"
	}
	verb := "is a build argument"
	if len(e.Keys) > 1 {
		verb = "are build arguments"
	}
	return fmt.Sprintf("%s %s of this service's image, which was built from source uploaded with `%s`, "+
		"and that source is not kept to build again. This change needs a new `%s`: "+
		"run it where the source is, and the build takes the new value. The change stays staged until then.",
		strings.Join(e.Keys, ", "), verb, up, up)
}

// argsUseAny reports whether a build given these arguments used any of the
// keys. Nil is a build that did not say, which is taken as yes.
func argsUseAny(args, keys []string) bool {
	if args == nil || slices.Contains(args, orchestrator.BuildArgsAll) {
		return true
	}
	for _, k := range keys {
		if slices.Contains(args, k) {
			return true
		}
	}
	return false
}
