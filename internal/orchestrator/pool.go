package orchestrator

import (
	"fmt"
	"regexp"
)

// PoolLabel is the node label a pool is carried in, and the key of the taint
// that keeps a dedicated pool's machines for its own apps.
//
// Here rather than beside the join command that writes it, because two
// things read it that have nothing to do with joining: the orchestrator,
// which places a team's apps by it, and the app service, which validates the
// pool a team is given before a workload is rendered with it.
const PoolLabel = "yacht/pool"

// SiteLabel is the node label a site is carried in: Kubernetes' own zone
// label, which the scheduler already spreads pods by.
const SiteLabel = "topology.kubernetes.io/zone"

// A Kubernetes label value. The same shape keeps a pool or a site safe to
// interpolate into the join command somebody pastes into a root shell: every
// character that could end that command and start another one — a semicolon,
// a backtick, a newline, a space, a dollar — is outside this set already.
var labelValueRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9._]*[a-z0-9])?$`)

// ValidatePool checks a pool name. Empty is no pool, which is valid: a node
// with no label, or a team whose apps run anywhere.
func ValidatePool(pool string) error { return ValidateLabelValue("pool", pool) }

// ValidateLabelValue checks a name that becomes a node label value, saying
// which kind of name it is in the message. Empty is valid: no label.
func ValidateLabelValue(what, v string) error {
	if v == "" {
		return nil
	}
	if len(v) > 63 {
		return fmt.Errorf("%s name must be at most 63 characters", what)
	}
	if !labelValueRE.MatchString(v) {
		return fmt.Errorf(
			"%s name must be lowercase letters, numbers, dashes, dots and underscores, "+
				"starting and ending with a letter or number", what)
	}
	return nil
}
