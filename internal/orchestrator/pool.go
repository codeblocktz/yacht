package orchestrator

import (
	"errors"
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

// A Kubernetes label value. The same shape keeps a pool safe to interpolate
// into the join command somebody pastes into a root shell: every character
// that could end that command and start another one — a semicolon, a
// backtick, a newline, a space, a dollar — is outside this set already.
var poolRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9._]*[a-z0-9])?$`)

// ValidatePool checks a pool name. Empty is no pool, which is valid: a node
// with no label, or a team whose apps run anywhere.
func ValidatePool(pool string) error {
	if pool == "" {
		return nil
	}
	if len(pool) > 63 {
		return errors.New("pool name must be at most 63 characters")
	}
	if !poolRE.MatchString(pool) {
		return errors.New(
			"pool name must be lowercase letters, numbers, dashes, dots and underscores, " +
				"starting and ending with a letter or number")
	}
	return nil
}
