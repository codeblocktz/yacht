package orchestrator

import (
	"context"
	"fmt"
	"io"
)

// ShellCommand is what a console runs in the container: bash where the image
// has it, sh where it does not.
//
// Decided inside the container rather than by asking first, because the only
// way to ask is a second exec, and an image's PATH is the image's own business.
// Most slim images carry sh and nothing else; the ones that carry bash are the
// ones whose users expect line editing and history to work.
var ShellCommand = []string{"/bin/sh", "-c", "command -v bash >/dev/null && exec bash || exec sh"}

// TerminalSize is a terminal's width and height in character cells.
type TerminalSize struct {
	Cols uint16
	Rows uint16
}

// ExecOptions is one interactive shell in one of an app's containers.
type ExecOptions struct {
	// Ref is the app. Its namespace is the only one searched and its name is
	// the label a pod must carry, so the pod has to belong to this app — not
	// merely sit in a namespace the caller can name.
	Ref Ref

	// Pod is which replica. It arrives from a request, and an implementation
	// must refuse one that is not Ref's: a pod name on its own is enough to
	// reach any tenant's container.
	Pod string

	// Stdin is what was typed, Stdout what the terminal shows. A terminal has
	// no separate error stream — the TTY merges them, as it does over SSH.
	Stdin  io.Reader
	Stdout io.Writer

	// Resize delivers the terminal's size each time it changes. Nil leaves the
	// container's default; closing it stops resizing without ending the shell.
	Resize <-chan TerminalSize
}

// Execer opens an interactive shell in a running container.
//
// Optional, and asserted for like CertificateReader. Everything else here is
// a read or a convergence toward declared state; this is a person acting
// inside a workload, and an implementation that cannot offer it — or a
// wrapping application that will not — simply does not satisfy this.
//
// Exec does not escalate anything. The shell runs as the container's own user
// under the container's own security context, which the namespace's pod
// security admission already holds to the restricted profile: whatever the app
// could not do, the console cannot either.
type Execer interface {
	// Exec runs ShellCommand in opts.Pod and relays the streams until the shell
	// exits or ctx is done. A shell that exits, whatever its status, returns
	// nil: typing exit is how a console is supposed to end.
	//
	// It returns ErrNotFound for a pod that is not the app's, and ErrNotStarted
	// for one whose container is not running.
	Exec(ctx context.Context, opts ExecOptions) error
}

// Compile-time check that Noop offers a console, if only to refuse it.
var _ Execer = (*Noop)(nil)

// Exec refuses, saying why. Without a cluster there is no container to open a
// shell in, and pretending otherwise — an echo, a fake prompt — would make an
// in-memory install look like it runs something.
func (n *Noop) Exec(context.Context, ExecOptions) error {
	return fmt.Errorf("%w: a console is not available — this install runs no "+
		"containers, so there is nothing to open a shell in", ErrNotSupported)
}
