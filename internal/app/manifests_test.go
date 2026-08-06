package app

import (
	"github.com/codeblocktz/yacht/internal/registry"
)

// The registry store is what actually answers Manifests.
//
// Asserted here rather than left to the wiring in main, because an interface
// the real implementation has drifted away from compiles fine everywhere
// except the one line that assembles the program — and that line is not
// covered by any test.
var _ Manifests = (*registry.Store)(nil)

// And Images, for the same reason: both interfaces describe the same object,
// and a change to one that quietly excludes it should fail here.
var _ Images = (*registry.Store)(nil)
