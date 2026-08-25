package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"syscall"
	"time"
)

// ProcessSupervisor runs one child in its own process group and terminates
// the whole group (TERM then, after TermGrace, KILL) when ctx is cancelled.
//
// Env, if non-nil, is the exact environment the child receives (this is the
// only place a Secret Broker grant may be merged into a child's environment;
// nil means "inherit exec.Command's default", matching earlier behaviour).
//
// Confine, if non-nil, runs the child inside a rootless user+mount
// namespace confined to Confine.Workspace (see NamespaceConfinement's doc
// comment for the mechanism). Run fails closed -- it returns
// ErrNamespaceUnsupported and never starts the child -- when the kernel or
// environment cannot actually provide that confinement, rather than
// silently falling back to running the child unconfined.
//
// Stdin and Stdout are additive (V2-017): Stdin, if non-nil, is written to
// the child's standard input; Stdout, if non-nil, receives the child's
// standard output. Both are zero-value-compatible with every existing
// caller (nil Stdin/Stdout leaves the child's stdin/stdout exactly as
// os/exec.Cmd's own zero value does today: an already-closed stdin and a
// discarded stdout). Neither field changes Run's signature or the
// TERM-then-KILL process-group logic below.
type ProcessSupervisor struct {
	TermGrace time.Duration
	Env       []string
	Confine   *NamespaceConfinement
	Stdin     []byte
	Stdout    io.Writer
}

func (s ProcessSupervisor) Run(ctx context.Context, argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("process argv is required")
	}
	if s.Confine != nil {
		if err := s.Confine.Probe(ctx); err != nil {
			return err
		}
		wrapped, err := s.Confine.wrap(argv)
		if err != nil {
			return err
		}
		argv = wrapped
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if s.Env != nil {
		cmd.Env = s.Env
	}
	if s.Stdin != nil {
		cmd.Stdin = bytes.NewReader(s.Stdin)
	}
	if s.Stdout != nil {
		cmd.Stdout = s.Stdout
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	case <-ctx.Done():
	}
	grace := s.TermGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-done:
		return ctx.Err()
	case <-time.After(grace):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return ctx.Err()
	}
}
