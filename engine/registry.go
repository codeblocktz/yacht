package engine

import (
	"context"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/registry"
)

// RegistrySettings is the install's image registry as stored, without its
// password: where builds push, and the account they push and pull under.
type RegistrySettings = registry.Settings

// ErrRegistryNoSecretKey means a registry password was offered with no
// YACHT_SECRET_KEY to seal it: it is refused rather than stored readable.
var ErrRegistryNoSecretKey = registry.ErrNoSecretKey

// Registry is an image registry for builds, as Settings › Registry takes it.
type Registry struct {
	// Host is the registry's hostname, with a port if not 443: no scheme,
	// no path.
	Host string
	// Repository is the path images are pushed under, empty for none.
	Repository string
	Username   string
	Password   string
	// Insecure means plain HTTP. The engine can only push that way: pulling
	// so is the nodes' configuration, which it does not touch.
	Insecure bool
}

// RegistrySettings is the registry builds push to now. An install with none
// has a zero RegistrySettings, not an error.
func (e *Engine) RegistrySettings(ctx context.Context) (RegistrySettings, error) {
	return e.registry.Settings(ctx)
}

// SetRegistry stores the registry builds push to and the cluster pulls from,
// as Settings › Registry does: for a wrapper that runs a registry of its own
// and fills the settings in from its configuration, rather than have an
// operator type them. The form's checks apply, and nothing is written when
// nothing differs, so calling it at every start costs nothing and leaves every
// Git app as it was. A change re-versions every Git app, as the form's does,
// so each is converged onto the new pull credential. Needs YACHT_SECRET_KEY:
// ErrRegistryNoSecretKey otherwise.
func (e *Engine) SetRegistry(ctx context.Context, r Registry) error {
	return e.registry.Set(ctx, uuid.Nil, r.Host, r.Repository, r.Username, r.Password, r.Insecure)
}
