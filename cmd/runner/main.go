package main

// cmd/runner (V2-091: it stops being a stub).
//
// WHAT IT WAS. Measured at 848d899, this binary refused to start without
// --fake, with the message "no external control-plane wiring is enabled", and
// that was HONEST: no file in internal/runner imported net/http, so there was
// no wiring to enable. The only journey the package could drive was an
// in-process one holding a pointer to the application Service, which is not a
// second process at all.
//
// WHAT IT IS NOW. --real drives one bounded pass over the work a Control Plane
// OFFERED it, over real HTTP, as a session the server verified, and STOPS at
// the provider boundary. It posts no Execution result, because all three
// provider adapters return provider.NoExec and no Provider CLI invocation is
// authorised: a result posted here would be a result this process did not
// obtain.
//
// WHAT IT DELIBERATELY DOES NOT IMPORT. No store. Measured in
// ci/components.json, the store-firestore component declares `runner` among its
// dependencies, so cmd/runner importing internal/store/firestore would be a
// CYCLE in the component graph that verification_dependencies exists to
// prevent; and internal/store/memory in a shipped binary is the fake this task
// refuses. cmd/runner imports internal/runner, which is INTRA-component -- the
// runner component's roots already contain both cmd/runner/** and
// internal/runner/** -- so ci/components.json is not edited.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/runner"
)

const version = "0.1.0-dev"

// The exit statuses, each named. A caller that has to tell "the data root was
// wrong" from "the control plane refused" cannot do it from a single status.
const (
	exitUsage           = 2
	exitDataRoot        = 3
	exitSessionToken    = 4
	exitControlPlane    = 5
	exitRunnerInternal  = 1
	requestTimeoutLimit = 30 * time.Second
)

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

func main() {
	// The --version branch stays FIRST and stays byte-identical in behaviour:
	// `make smoke` greps this exact line on stdout, so no work of any kind may
	// happen before it.
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(version)
		return
	}
	flags := flag.NewFlagSet("runner", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	fake := flags.Bool("fake", false, "run the explicit local fake daemon")
	real := flags.Bool("real", false, "drive one bounded pass against a real control plane over HTTP")
	dataRoot := flags.String("data-root", "", "absolute 0700 local data root")
	runnerID := flags.String("runner-id", "", "runner identity (required for --fake)")
	controlPlane := flags.String("control-plane", "", "control plane base URL, e.g. http://127.0.0.1:8080 (required for --real)")
	// The token is named by PATH and never by value: a token passed as a flag
	// value is visible in every process listing on the machine, and a token
	// passed in an environment variable is inherited by every child process.
	sessionTokenFile := flags.String("session-token-file", "", "path of a file, no wider than 0600, holding the runner session token (required for --real)")
	maxClaims := flags.Int("max-claims", runner.MaxDriverClaims, "maximum Increments this pass may claim")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(exitUsage)
	}
	if *fake && *real {
		fmt.Fprintln(os.Stderr, "runner: --fake and --real are mutually exclusive")
		os.Exit(exitUsage)
	}
	if *real {
		if err := runReal(*dataRoot, *controlPlane, *sessionTokenFile, *maxClaims); err != nil {
			// The error is printed as-is. Every error the client and the driver
			// produce is asserted token-free in
			// internal/runner/controlplane_test.go, so this line cannot leak the
			// session token.
			fmt.Fprintln(os.Stderr, "runner:", err)
			os.Exit(exitStatusFor(err))
		}
		return
	}
	// The pre-existing --fake mode and its refusal messages are preserved
	// byte-for-byte: cmd/runner/main_test.go asserts each string.
	if !*fake {
		fmt.Fprintln(os.Stderr, "runner: no external control-plane wiring is enabled; use --fake explicitly for local mode")
		os.Exit(exitUsage)
	}
	if *runnerID == "" {
		fmt.Fprintln(os.Stderr, "runner: --runner-id is required with --fake")
		os.Exit(exitUsage)
	}
	root := *dataRoot
	if root == "" {
		var err error
		root, err = os.MkdirTemp("", "agentic-runner-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "runner:", err)
			os.Exit(exitRunnerInternal)
		}
		defer os.RemoveAll(root)
		fmt.Fprintln(os.Stderr, "WARNING: explicit --fake mode uses temporary memory/local state; no external provider is connected")
	}
	if err := validateDataRoot(root); err != nil {
		fmt.Fprintln(os.Stderr, "runner:", err)
		os.Exit(exitUsage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "runner: fake daemon ready runner_id=%s data_root=%s\n", *runnerID, root)
	<-ctx.Done()
}

// errDataRoot, errSessionToken and errControlPlane classify a refusal so each
// one gets its OWN exit status. They carry no value from the file they refused.
var (
	errDataRoot      = errors.New("data root")
	errSessionToken  = errors.New("session token")
	errControlPlaneC = errors.New("control plane")
)

func exitStatusFor(err error) int {
	switch {
	case errors.Is(err, errDataRoot):
		return exitDataRoot
	case errors.Is(err, errSessionToken):
		return exitSessionToken
	case errors.Is(err, errControlPlaneC):
		return exitControlPlane
	default:
		return exitRunnerInternal
	}
}

// runReal validates every input explicitly -- nothing is defaulted -- and then
// drives exactly ONE bounded pass.
func runReal(dataRoot, base, sessionTokenFile string, maxClaims int) error {
	if dataRoot == "" {
		return fmt.Errorf("%w: --data-root is required with --real; it is never a temporary directory in real mode", errDataRoot)
	}
	if !filepath.IsAbs(dataRoot) {
		return fmt.Errorf("%w: --data-root must be absolute", errDataRoot)
	}
	// The EXISTING validateDataRoot is reused unchanged -- it is the function
	// --fake has always used -- but it CHMODS the root to 0700 rather than
	// refusing a wider one, which is right for a mode that creates its own
	// temporary directory and wrong for a mode that is handed an existing one.
	// Real mode therefore REFUSES a wider root before calling it, rather than
	// silently tightening a directory the owner already put something in. This
	// check is additive: --fake's behaviour is byte-unchanged.
	if info, err := os.Stat(dataRoot); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%w: --data-root is not a directory", errDataRoot)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			return fmt.Errorf("%w: --data-root permissions are %04o; real mode requires exactly 0700 and will not tighten a directory it was handed", errDataRoot, perm)
		}
	}
	if err := validateDataRoot(dataRoot); err != nil {
		return fmt.Errorf("%w: %v", errDataRoot, err)
	}
	if base == "" {
		return fmt.Errorf("%w: --control-plane is required with --real; a defaulted origin is how a Runner ends up talking to the wrong installation", errControlPlaneC)
	}
	if sessionTokenFile == "" {
		return fmt.Errorf("%w: --session-token-file is required with --real", errSessionToken)
	}
	// The token file is validated HERE, before anything else is built, so the
	// refusal is early and its message names the PATH and never the contents.
	// The validation is runner.ReadSessionTokenFile itself rather than a second
	// expression of "no wider than 0600", so the two cannot drift.
	if _, err := runner.ReadSessionTokenFile(sessionTokenFile); err != nil {
		return fmt.Errorf("%w: %v", errSessionToken, err)
	}
	if maxClaims <= 0 {
		return fmt.Errorf("%w: --max-claims must be positive", errControlPlaneC)
	}
	if maxClaims > runner.MaxDriverClaims {
		return fmt.Errorf("%w: --max-claims must not exceed the declared bound of %d", errControlPlaneC, runner.MaxDriverClaims)
	}
	workspaceRoot := filepath.Join(dataRoot, "workspaces")
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		return fmt.Errorf("%w: %v", errDataRoot, err)
	}
	workspace, err := runner.NewWorkspace(workspaceRoot)
	if err != nil {
		return fmt.Errorf("%w: %v", errDataRoot, err)
	}
	journal, err := runner.OpenJournal(filepath.Join(dataRoot, "journal"))
	if err != nil {
		return fmt.Errorf("%w: %v", errDataRoot, err)
	}
	client, err := runner.NewControlPlaneClient(runner.ControlPlaneConfig{
		BaseURL:          base,
		SessionTokenPath: sessionTokenFile,
		// An EXPLICIT bounded timeout. http.DefaultClient's Timeout is zero,
		// which is an unbounded request a Runner cannot recover from.
		HTTPClient: &http.Client{Timeout: requestTimeoutLimit},
		Clock:      wallClock{},
	})
	if err != nil {
		return fmt.Errorf("%w: %v", errControlPlaneC, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	driver := &runner.LoopDriver{
		Client:           client,
		Workspace:        workspace,
		Journal:          journal,
		RequestNamespace: "runner-pass:" + filepath.Base(dataRoot),
	}
	report, err := driver.RunOnce(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", errControlPlaneC, err)
	}
	// One line on stdout, identifiers only: no provider output, no session
	// token, no requirement text. It is what the preview-local exercise reads.
	fmt.Printf("runner: pass complete offered=%d claimed=%d deferred=%d heartbeats=%d stopped_at_provider_boundary=%v\n",
		report.Offered, len(report.Claimed), report.Deferred, report.Heartbeats, report.StoppedAtProviderBoundary)
	for _, claim := range report.Claimed {
		fmt.Printf("runner: claimed requirement_id=%s increment_id=%s execution_id=%s execution_state=%s\n",
			claim.RequirementID, claim.IncrementID, claim.ExecutionID, claim.ExecutionState)
	}
	return nil
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
