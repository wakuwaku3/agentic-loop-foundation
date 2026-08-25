// bootstrap is the Bootstrapper: the one component that is deliberately not
// self-updating (docs/operations/self-update.md section 4.3). It installs a
// signed Runner bundle side by side, moves a channel pointer, and launches
// the Runner from a channel pointer after re-verifying the bytes on disk.
//
// There is no --public-key flag. The trust anchor is resolved from a fixed
// path under the machine root by internal/update, because a Bootstrapper
// that asks its caller for its own trust anchor has a decorative signature
// check: whoever chooses the key decides what "valid" means.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/runner"
	"github.com/takushi/agentic-loop-foundation/v2/internal/update"
)

const usage = "usage: bootstrap install|switch|run|--version"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("0.1.0-dev")
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "bootstrap:", err)
		os.Exit(2)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "install":
		return runInstall(args[1:], out)
	case "switch":
		return runSwitch(args[1:])
	case "run":
		return runLaunch(ctx, args[1:], out)
	default:
		return errors.New(usage)
	}
}

func runInstall(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("bootstrap install", flag.ContinueOnError)
	root := flags.String("root", "", "absolute bootstrap root")
	manifestPath := flags.String("manifest", "", "signed manifest path")
	binaryPath := flags.String("binary", "", "runner binary path")
	signaturePath := flags.String("signature", "", "raw Ed25519 signature path")
	schema := flags.Int("schema", 0, "current canonical schema version")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *schema <= 0 {
		return errors.New("invalid install arguments")
	}
	anchors, err := update.NewAnchorResolver(*root).Resolve()
	if err != nil {
		return err
	}
	manifest, err := os.ReadFile(*manifestPath)
	if err != nil {
		return err
	}
	binary, err := os.ReadFile(*binaryPath)
	if err != nil {
		return err
	}
	signature, err := os.ReadFile(*signaturePath)
	if err != nil {
		return err
	}
	state, err := update.LoadState(*root, *schema)
	if err != nil {
		return err
	}
	state.CanonicalSchema = *schema
	installed, err := update.InstallRecorded(*root, state, update.Bundle{Manifest: manifest, Binary: binary, Signature: signature}, anchors, time.Now().UTC())
	if err != nil {
		return err
	}
	fmt.Fprintln(out, installed.Version)
	return nil
}

func runSwitch(args []string) error {
	flags := flag.NewFlagSet("bootstrap switch", flag.ContinueOnError)
	root := flags.String("root", "", "absolute bootstrap root")
	channel := flags.String("channel", "", "stable or preview")
	version := flags.String("version", "", "installed semantic version")
	direction := flags.String("direction", string(update.SwitchForward), "forward or rollback")
	reason := flags.String("reason", "", "why this machine is being re-routed")
	candidate := flags.String("candidate", "", "gate-passed candidate id a forward move names")
	schema := flags.Int("schema", 0, "current canonical schema version")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *schema <= 0 {
		return errors.New("invalid switch arguments")
	}
	state, err := update.LoadState(*root, *schema)
	if err != nil {
		return err
	}
	state.CanonicalSchema = *schema
	return update.Switch(*root, state, update.SwitchRequest{
		Channel:         *channel,
		Version:         *version,
		Direction:       update.SwitchDirection(*direction),
		Reason:          *reason,
		CandidateDigest: *candidate,
	}, time.Now().UTC())
}

func runLaunch(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("bootstrap run", flag.ContinueOnError)
	root := flags.String("root", "", "absolute bootstrap root")
	channel := flags.String("channel", "", "stable or preview")
	schema := flags.Int("schema", 0, "current canonical schema version")
	launches := flags.Int("launches", 1, "how many verified launches to perform")
	workspace := flags.String("workspace", "", "absolute workspace directory the child keeps writable")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *schema <= 0 {
		return errors.New("invalid run arguments")
	}
	confinement := runner.NamespaceConfinement{Workspace: *workspace}
	launcher := update.Launcher{
		Root:            *root,
		CanonicalSchema: *schema,
		Anchors:         update.NewAnchorResolver(*root),
		Probe:           confinement.Probe,
		Start:           processStarter{stdout: os.Stdout, stderr: os.Stderr}.start,
		Args:            flags.Args(),
	}
	outcome, err := launcher.Run(ctx, *channel, *launches)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s %s verifications=%d launches=%d\n", outcome.Channel, outcome.Version, outcome.Verifications, outcome.Launches)
	return nil
}
