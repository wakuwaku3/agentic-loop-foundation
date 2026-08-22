package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

const version = "0.1.0-dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(version)
		return
	}
	flags := flag.NewFlagSet("runner", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	fake := flags.Bool("fake", false, "run the explicit local fake daemon")
	dataRoot := flags.String("data-root", "", "absolute 0700 local data root")
	runnerID := flags.String("runner-id", "", "runner identity (required for --fake)")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if !*fake {
		fmt.Fprintln(os.Stderr, "runner: no external control-plane wiring is enabled; use --fake explicitly for local mode")
		os.Exit(2)
	}
	if *runnerID == "" {
		fmt.Fprintln(os.Stderr, "runner: --runner-id is required with --fake")
		os.Exit(2)
	}
	root := *dataRoot
	if root == "" {
		var err error
		root, err = os.MkdirTemp("", "agentic-runner-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "runner:", err)
			os.Exit(1)
		}
		defer os.RemoveAll(root)
		fmt.Fprintln(os.Stderr, "WARNING: explicit --fake mode uses temporary memory/local state; no external provider is connected")
	}
	if err := validateDataRoot(root); err != nil {
		fmt.Fprintln(os.Stderr, "runner:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "runner: fake daemon ready runner_id=%s data_root=%s\n", *runnerID, root)
	<-ctx.Done()
}

func validateDataRoot(root string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("data root must be absolute")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	if err := os.Chmod(root, 0700); err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("data root must be a 0700 directory")
	}
	return nil
}
