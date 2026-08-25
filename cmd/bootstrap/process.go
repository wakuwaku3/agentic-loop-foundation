package main

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"syscall"

	"github.com/takushi/agentic-loop-foundation/v2/internal/update"
)

// processStarter is the production implementation of the launcher's process
// port. The child runs in its own process group -- internal/runner's
// ProcessSupervisor sets Setpgid for the same reason -- so a signal aimed at
// the child's group never reaches the launcher, and the launcher stays alive
// to re-verify and re-launch after the child exits.
type processStarter struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	// afterStart is a test seam, observed while the child is still alive.
	// Production leaves it nil.
	afterStart func(*exec.Cmd) error
}

func (p processStarter) start(ctx context.Context, spec update.ChildSpec) (update.ChildResult, error) {
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	if spec.NewProcessGroup {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	cmd.Stdin = p.stdin
	cmd.Stdout = p.stdout
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		return update.ChildResult{}, err
	}
	if p.afterStart != nil {
		if err := p.afterStart(cmd); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return update.ChildResult{}, err
		}
	}
	err := cmd.Wait()
	if err == nil {
		return update.ChildResult{ExitCode: 0}, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return update.ChildResult{ExitCode: exit.ExitCode()}, nil
	}
	return update.ChildResult{}, err
}
