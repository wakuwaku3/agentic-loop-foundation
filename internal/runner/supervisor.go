package runner

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

type ProcessSupervisor struct{ TermGrace time.Duration }

func (s ProcessSupervisor) Run(ctx context.Context, argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("process argv is required")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
