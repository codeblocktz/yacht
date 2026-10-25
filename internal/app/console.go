package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// ErrConsoleUnavailable means this install cannot open a shell in a container
// at all: its orchestrator has no way to, or a wrapping application has chosen
// not to offer one.
var ErrConsoleUnavailable = errors.New("app: a console is not available on this install")

// ErrConsoleAsleep means the app is asleep, or on its way back. A sleeping app
// runs no containers, so there is nothing to open a shell in until it wakes —
// and the console does not wake it, because opening a terminal is not a
// request somebody would expect to start an app and bill for it.
var ErrConsoleAsleep = errors.New("app: the app is asleep — wake it first to open a console")

// ErrConsoleNoPods means the app has no running container to open a shell in:
// scaled to zero, never deployed, or every replica still starting.
var ErrConsoleNoPods = errors.New("app: the app has no running pods to open a console in")

// ConsoleRequest asks for a shell in one of an app's containers.
type ConsoleRequest struct {
	// Pod is optional. Empty takes the app's first running pod, which is what
	// somebody opening a single-replica service wants. A named one is honoured
	// only if it is one of this app's.
	Pod string

	// Actor is who is asking, for the record of the session. It decides
	// nothing: authorisation is the owner the app was looked up by, and this
	// is only the name written beside it.
	Actor string
}

// Terminal is the far end of a console: what was typed, where output goes,
// and the terminal's size as it changes.
type Terminal struct {
	Stdin  io.Reader
	Stdout io.Writer
	Resize <-chan orchestrator.TerminalSize
}

// ConsoleSession is a shell resolved and ready to start.
//
// Split from Console so the refusals — another team's app, a pod that is not
// this app's, an app asleep — are answered before the caller commits to a
// stream. A handler can turn them into an HTTP status or a message, and only
// once nothing is left to refuse does it upgrade the connection and Run.
type ConsoleSession interface {
	// Pod is the pod the shell will open in.
	Pod() string

	// Pods is every pod of the app a console could open in, for a picker.
	Pods() []string

	// Run starts the shell and relays the terminal until the shell exits or
	// ctx is done.
	Run(ctx context.Context, t Terminal) error
}

// Console resolves a shell in one of an app's containers.
//
// The app is looked up by owner and the namespace comes from its row, exactly
// as Logs does it: the only pods reachable are an app the caller owns. The pod
// name is checked against that app's own pods, never trusted from the request,
// and the orchestrator checks it again against the pod's labels before it opens
// anything. Two checks for one boundary is deliberate here — a console is
// write access to a running container, and a guard that is one forgotten line
// away from gone is not a guard.
func (s *Service) Console(
	ctx context.Context, ownerID, appName string, req ConsoleRequest,
) (ConsoleSession, error) {
	execer, ok := s.orch.(orchestrator.Execer)
	if !ok {
		return nil, ErrConsoleUnavailable
	}

	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return nil, err
	}
	if a.Sleep.Asleep() {
		return nil, ErrConsoleAsleep
	}

	pods, err := s.orch.Pods(ctx, orchestrator.PodListOptions{
		Namespace: a.Namespace, ManagedOnly: true, Owner: orchestrator.OwnerID(ownerID),
	})
	if err != nil {
		return nil, fmt.Errorf("app: list pods for console: %w", err)
	}

	sess := &consoleSession{svc: s, execer: execer, ref: a.Ref(), owner: ownerID, actor: req.Actor}
	var running []string
	for _, p := range pods {
		// The app's own pods, not every pod the owner has in the namespace. A
		// namespace is per app today, but this is the line that keeps that an
		// implementation detail rather than the boundary.
		if p.App != a.Name {
			continue
		}
		sess.pods = append(sess.pods, p.Name)
		if p.Phase == "Running" {
			running = append(running, p.Name)
		}
	}

	if req.Pod != "" {
		for _, name := range sess.pods {
			if name == req.Pod {
				sess.pod = name
				return sess, nil
			}
		}
		// Not one of this app's. The same answer as an app that does not
		// exist, so the name is not a way to find out what else is running.
		return nil, ErrNotFound
	}
	if len(running) == 0 {
		return nil, ErrConsoleNoPods
	}
	sess.pod = running[0]
	return sess, nil
}

// consoleSession is the service's ConsoleSession.
type consoleSession struct {
	svc    *Service
	execer orchestrator.Execer
	ref    orchestrator.Ref
	pod    string
	pods   []string
	owner  string
	actor  string
}

func (c *consoleSession) Pod() string    { return c.pod }
func (c *consoleSession) Pods() []string { return c.pods }

// Run opens the shell, and records that it was opened and when it closed.
//
// Who, which app, which pod, and for how long — and never what was typed or
// shown. A console is where people paste tokens and read environment
// variables, and a record of the keystrokes would be a second copy of every
// secret anyone ever looked at, kept somewhere with weaker access than the
// container it came from.
func (c *consoleSession) Run(ctx context.Context, t Terminal) error {
	attrs := []any{
		slog.String("owner", c.owner),
		slog.String("actor", c.actor),
		slog.String("app", c.ref.Name),
		slog.String("namespace", c.ref.Namespace),
		slog.String("pod", c.pod),
	}
	started := time.Now()
	c.svc.log.Info("console opened", attrs...)

	err := c.execer.Exec(ctx, orchestrator.ExecOptions{
		Ref: c.ref, Pod: c.pod,
		Stdin: t.Stdin, Stdout: t.Stdout, Resize: t.Resize,
	})

	attrs = append(attrs, slog.Duration("duration", time.Since(started).Round(time.Second)))
	switch {
	case err == nil, ctx.Err() != nil:
		// A closed tab and a shell that exited are both ordinary ends. The
		// caller's context ending is how every console it times out ends too,
		// and those are the caller's to say why.
		c.svc.log.Info("console closed", attrs...)
		return nil
	default:
		c.svc.log.Warn("console failed", append(attrs, slog.String("error", err.Error()))...)
		return err
	}
}
