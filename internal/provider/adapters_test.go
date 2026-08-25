package provider_test

// Adapter argv surface and the nine-case contract-fixture matrix
// (V2-027 A3-A6).

import (
	"context"
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

// helpDeclaredFlags is the flag surface each CLI's own help output declares,
// transcribed from a measurement taken with help only -- codex --version,
// opencode --version, codex exec --help and opencode run --help. No
// subcommand of any Provider CLI was executed: reading help consumes no
// Provider usage and needs no authentication, and running one would consume
// both. The verbatim help lines are quoted in
// docs/operations/provider-adapters.md.
//
// claude's entry is the one measured elsewhere: its four arguments are
// live-proven wire-compatible against CLI version 2.1.241 by three separate
// live exercises, so this task changes nothing about it and asserts only that
// it is unchanged.
var helpDeclaredFlags = map[string]map[string]bool{
	"codex": {
		// "  -C, --cd <DIR>" and "      --ephemeral" and "      --json"
		"-C": true, "--cd": true, "--ephemeral": true, "--json": true,
		"--config": true, "-c": true, "--model": true, "-m": true,
		"--sandbox": true, "-s": true, "--skip-git-repo-check": true,
		"--output-schema": true, "--color": true, "--output-last-message": true,
		"-o": true, "--ignore-user-config": true, "--add-dir": true,
	},
	"claude": {
		"--print": true, "--output-format": true, "--no-session-persistence": true,
	},
	"opencode": {
		// "      --pure         run without external plugins" and
		// "      --format       format: default (formatted) or json (raw JSON events)"
		// and "      --dir          directory to run in, path on remote server if attaching"
		"--pure": true, "--format": true, "--dir": true, "--print-logs": true,
		"--log-level": true, "--command": true, "--agent": true, "--model": true,
		"-m": true, "--title": true, "--variant": true,
	},
}

// wantArgv is the exact argv each adapter builds for a request whose
// workspace is /workspace. Nothing here is new: this task changes no flag on
// any of the three adapters. It pins them so a later change is visible.
var wantArgv = map[string][]string{
	"codex":    {"codex", "exec", "--json", "--ephemeral", "-C", "/workspace"},
	"claude":   {"claude", "--print", "--output-format", "json", "--no-session-persistence"},
	"opencode": {"opencode", "run", "--pure", "--format", "json", "--dir", "/workspace"},
}

// workspaceFlag names, per adapter, the measured directory flag the workspace
// argument follows. claude has none: it carries the Work Packet on stdin.
var workspaceFlag = map[string]string{"codex": "-C", "claude": "", "opencode": "--dir"}

func allAdapters() []provider.Adapter {
	return []provider.Adapter{provider.CodexAdapter{}, provider.ClaudeAdapter{}, provider.OpenCodeAdapter{}}
}

// TestAdapterArgvIsExactlyWhatHelpDeclares is A3. Every flag-shaped argv
// element must appear in the flag set that CLI's own help output declares,
// and the whole argv must equal the pinned surface.
func TestAdapterArgvIsExactlyWhatHelpDeclares(t *testing.T) {
	for _, a := range allAdapters() {
		inv, err := a.Build(provider.Request{OperationID: "op-1", Workspace: "/workspace", Packet: packet()})
		if err != nil {
			t.Fatalf("%s: build: %v", a.Name(), err)
		}
		want := wantArgv[a.Name()]
		if len(inv.Argv) != len(want) {
			t.Fatalf("%s: argv = %v, want %v", a.Name(), inv.Argv, want)
		}
		for i := range want {
			if inv.Argv[i] != want[i] {
				t.Fatalf("%s: argv[%d] = %q, want %q (full argv %v)", a.Name(), i, inv.Argv[i], want[i], inv.Argv)
			}
		}
		declared := helpDeclaredFlags[a.Name()]
		flagCount := 0
		for _, element := range inv.Argv[1:] {
			if !strings.HasPrefix(element, "-") {
				continue
			}
			flagCount++
			if !declared[element] {
				t.Fatalf("%s: argv carries %q, which this CLI's help output does not declare; a flag help does not declare may not be invented", a.Name(), element)
			}
		}
		if flagCount == 0 {
			t.Fatalf("%s: argv declares no flag at all; the scan would pass vacuously", a.Name())
		}
		t.Logf("%s: argv=%v flags-checked=%d against the help-declared set", a.Name(), inv.Argv, flagCount)
	}
	// claude's four arguments specifically, named, because A3 requires
	// recording that they are unchanged.
	claude, err := (provider.ClaudeAdapter{}).Build(provider.Request{OperationID: "op-1", Workspace: "/workspace", Packet: packet()})
	if err != nil {
		t.Fatal(err)
	}
	if len(claude.Argv) != 5 {
		t.Fatalf("claude argv has %d elements; its four arguments after argv[0] are live-proven and must not change", len(claude.Argv)-1)
	}
}

// TestWorkspacePinningIsNotReExpressedAsAFlagBeyondTheMeasuredException is
// A4, asserted in the form the measurement supports.
//
// MEASURED DISCREPANCY, recorded rather than papered over. A4 as written asks
// for "no argv element is an absolute path other than argv[0]". That is not
// satisfiable without deleting a flag both CLIs' help output declares, and
// the flags stay -- but as of V2-077 they stay for a different reason than
// the one first recorded here.
//
// HISTORICAL MEASUREMENT, taken 2026-08-24 for V2-027 and true of the tree as
// it then stood (kept rather than deleted, because the conclusion below was
// reached from it):
//
//   - internal/runner/supervisor.go's ProcessSupervisor.Run took only a
//     context and an argv. It never assigned a working directory to the child.
//   - internal/runner/provider.go's SupervisedInvocationRunner did not read
//     Invocation.WorkingDirectory either; it read Argv and Stdin, and built
//     the child's environment itself.
//
// CURRENT MEASUREMENT, 2026-08-25 (V2-077): the first half is no longer true.
// ProcessSupervisor now carries an additive Dir field and assigns it to the
// child, and SupervisedInvocationRunner reads Invocation.WorkingDirectory,
// validates it fail-closed on five properties before any preflight load or
// ledger reservation, and sets it as the supervisor's Dir. So
// Invocation.WorkingDirectory IS consumed on the production path, and for
// codex and opencode the flag is no longer the only representation of the
// workspace that reaches the child.
//
// (Both measurements were, and are, silent on Invocation.Environment: the
// runner has never read it. It is set by internal/runner's Grant.Apply and
// observed only by FakeInvocationRunner, so a Secret Broker grant does not
// reach a real child. That is a second, same-shaped defect, recorded by
// V2-077 and owned by its follow-up, not fixed here.)
//
// The flags nevertheless stay, and the conclusion is unchanged -- but it now
// rests on an equality that is asserted mechanically rather than on the old
// premise. One call to the shared build helper produces both values from the
// same req.Workspace, so the flag's argument and WorkingDirectory are the
// same string by construction; for an absolute path equal to the working
// directory a directory flag is idempotent, so there is no relative-resolution
// difference for the double expression to expose. The equality is asserted for
// all three adapters by
// TestDirectoryFlagArgumentAndWorkingDirectoryAreTheSameString below, and the
// runner refuses fail-closed if the two ever disagree. Removing a flag the
// CLI's own help declares remains out of scope. What stays unmeasured is
// whether some future CLI version refuses when a directory flag and an
// inherited working directory are both supplied: measuring that needs the run
// subcommand executed, which help declares nothing about, and V2-028 owns it.
//
// The assertion is therefore made stronger where it can be, rather than
// weaker: the workspace may appear at most once, it must equal the request
// workspace exactly, it must immediately follow that adapter's measured
// directory flag, and every other argv element must be neither an absolute
// path nor a traversal. claude, which needs no such flag, must have exactly
// one absolute path in its argv: argv[0].
func TestWorkspacePinningIsNotReExpressedAsAFlagBeyondTheMeasuredException(t *testing.T) {
	const workspace = "/tmp/agentic-loop/workspace"
	for _, a := range allAdapters() {
		inv, err := a.Build(provider.Request{OperationID: "op-1", Workspace: workspace, Packet: packet()})
		if err != nil {
			t.Fatalf("%s: build: %v", a.Name(), err)
		}
		if inv.WorkingDirectory != workspace {
			t.Fatalf("%s: WorkingDirectory = %q, want the request workspace %q", a.Name(), inv.WorkingDirectory, workspace)
		}
		if strings.HasPrefix(inv.Argv[0], "/") {
			t.Fatalf("%s: argv[0] = %q is an absolute path; the adapter emits a bare executable name and the runner resolves it to the approved record's executable_path, refusing when the two disagree", a.Name(), inv.Argv[0])
		}
		absolute := 0
		workspaceElements := 0
		for i, element := range inv.Argv {
			if strings.ContainsAny(element, "\x00\r\n") {
				t.Fatalf("%s: argv[%d] carries a NUL, carriage return or newline", a.Name(), i)
			}
			for _, segment := range strings.Split(strings.ReplaceAll(element, "\\", "/"), "/") {
				if segment == ".." {
					t.Fatalf("%s: argv[%d] = %q carries a path traversal segment", a.Name(), i, element)
				}
			}
			if strings.HasPrefix(element, "/") {
				absolute++
			}
			if element != workspace {
				continue
			}
			workspaceElements++
			if i == 0 {
				t.Fatalf("%s: argv[0] is the workspace, not an executable", a.Name())
			}
			flag := workspaceFlag[a.Name()]
			if flag == "" {
				t.Fatalf("%s: argv carries the workspace but this adapter has no measured directory flag", a.Name())
			}
			if inv.Argv[i-1] != flag {
				t.Fatalf("%s: the workspace argument at argv[%d] does not follow the measured directory flag %q (argv[%d]=%q)", a.Name(), i, flag, i-1, inv.Argv[i-1])
			}
		}
		if workspaceElements > 1 {
			t.Fatalf("%s: the workspace appears %d times in argv; once is the measured exception, twice is a defect", a.Name(), workspaceElements)
		}
		wantAbsolute := 0
		if workspaceFlag[a.Name()] != "" {
			wantAbsolute = 1
		}
		if absolute != wantAbsolute {
			t.Fatalf("%s: argv carries %d absolute paths, want %d (the single measured workspace argument, if this adapter has one)", a.Name(), absolute, wantAbsolute)
		}
		if a.Name() == "claude" && absolute != 0 {
			t.Fatalf("claude argv carries %d absolute paths; it needs no directory flag and must carry none", absolute)
		}
		t.Logf("%s: WorkingDirectory=%q absolute-argv-elements=%d workspace-argv-elements=%d", a.Name(), inv.WorkingDirectory, absolute, workspaceElements)
	}

	// Build refuses a workspace carrying any of those, for all three.
	refused := []string{
		"/workspace/../../etc",
		"/workspace/..",
		"..",
		"/workspace\x00/etc",
		"/workspace\nrm -rf /",
		"/workspace\rmalicious",
	}
	for _, a := range allAdapters() {
		for _, bad := range refused {
			if _, err := a.Build(provider.Request{OperationID: "op-1", Workspace: bad, Packet: packet()}); err == nil {
				t.Fatalf("%s: Build accepted workspace %q", a.Name(), strings.ReplaceAll(bad, "\x00", "<NUL>"))
			}
		}
	}
	t.Log("what holds the boundary is the kernel: internal/runner.NamespaceConfinement pins the writable mount at the workspace and refuses to run the child at all when the kernel cannot provide the namespace. ProcessSupervisor runs the child in its own process group and terminates the whole group. The adapter's job is only to be unable to ask to leave, which is what the refusals above are")
}

// TestDirectoryFlagArgumentAndWorkingDirectoryAreTheSameString is V2-077 A7.
// It replaces the assumption that keeping both the directory flag and
// Invocation.WorkingDirectory is harmless with a proof, for the values that
// actually occur: one call to the shared build helper produces both from the
// same req.Workspace, so they must be the same string, and claude -- which
// help declares no directory flag for -- must carry no path in argv at all.
//
// No flag is removed, renamed or reordered here, and no Provider CLI
// subcommand is executed: this is a property of the built Invocation, read
// entirely in process.
func TestDirectoryFlagArgumentAndWorkingDirectoryAreTheSameString(t *testing.T) {
	const workspace = "/tmp/agentic-loop/workspace"
	for _, a := range allAdapters() {
		inv, err := a.Build(provider.Request{OperationID: "op-1", Workspace: workspace, Packet: packet()})
		if err != nil {
			t.Fatalf("%s: build: %v", a.Name(), err)
		}
		if inv.WorkingDirectory != workspace {
			t.Fatalf("%s: WorkingDirectory = %q, want the request workspace %q", a.Name(), inv.WorkingDirectory, workspace)
		}
		flag := workspaceFlag[a.Name()]
		if flag == "" {
			// claude: no directory flag, and therefore no path in argv.
			for i, element := range inv.Argv {
				if strings.HasPrefix(element, "/") {
					t.Fatalf("%s carries no directory flag, so argv must carry no path at all; argv[%d] = %q", a.Name(), i, element)
				}
			}
			t.Logf("%s: no directory flag, no path in argv, WorkingDirectory=%q is the only representation of the workspace", a.Name(), inv.WorkingDirectory)
			continue
		}
		index := -1
		for i, element := range inv.Argv {
			if element != flag {
				continue
			}
			if index != -1 {
				t.Fatalf("%s: the measured directory flag %q appears more than once in argv", a.Name(), flag)
			}
			index = i
		}
		if index == -1 {
			t.Fatalf("%s: the measured directory flag %q is absent from argv; V2-077 must not remove a flag the CLI's own help declares", a.Name(), flag)
		}
		if index+1 >= len(inv.Argv) {
			t.Fatalf("%s: the directory flag %q is the last argv element and carries no argument", a.Name(), flag)
		}
		argument := inv.Argv[index+1]
		if argument != inv.WorkingDirectory {
			t.Fatalf("%s: %s carries %q while WorkingDirectory is %q; both come from one call to the shared build helper, so they must be the same string", a.Name(), flag, argument, inv.WorkingDirectory)
		}
		t.Logf("%s: %s argument and WorkingDirectory are the same string (%q), so the double expression exposes no relative-resolution difference", a.Name(), flag, argument)
	}
}

// ---------------------------------------------------------------------------
// A5/A6: the nine-case contract-fixture matrix, as ONE table over three
// adapter values.
//
// The list of cases is validation.md's Provider adapter contract tests
// section taken verbatim, with two of its rows split because they name two
// distinguishable facts each ("empty／malformed structured output" and
// "usageが取得可能／不可能"), and with its timeout／cancel row deliberately
// absent from the file set: a timeout and a cancellation are conditions of
// the invocation, not shapes of an output, so they are driven through
// ClassifyError and NormalizeError instead (see
// TestTimeoutAndCancelAreConditionsOfTheInvocationNotShapesOfAnOutput).
//
// One table over three adapter values, rather than three near-identical
// tables, is the whole mechanism: a change to build, parseFixture or
// normalize has to reach all three rows or a cell fails.
// ---------------------------------------------------------------------------

type fixtureCase struct {
	name              string
	validationRow     string
	wantParseRefused  bool
	wantSucceeded     bool
	wantClass         provider.FailureClass
	wantRetryable     bool
	wantAmbiguous     bool
	wantUsageReported bool
	wantOutputDigest  bool
}

var fixtureMatrix = []fixtureCase{
	{name: "success", validationRow: "success", wantSucceeded: true, wantUsageReported: true, wantOutputDigest: true},
	{name: "model-error", validationRow: "explicit model error", wantClass: provider.FailureModel},
	{name: "usage-exhaustion", validationRow: "quota／usage exhaustion", wantClass: provider.FailureQuota, wantRetryable: true},
	{name: "non-zero-exit", validationRow: "non-zero exit", wantClass: provider.FailureTransport, wantRetryable: true},
	{name: "zero-exit-error-envelope", validationRow: "zero exit error envelope", wantClass: provider.FailureModel},
	{name: "empty-structured-output", validationRow: "empty／malformed structured output (empty half)", wantParseRefused: true, wantClass: provider.FailureContract},
	{name: "malformed-structured-output", validationRow: "empty／malformed structured output (malformed half)", wantParseRefused: true, wantClass: provider.FailureContract},
	{name: "usage-reported", validationRow: "usageが取得可能", wantSucceeded: true, wantUsageReported: true, wantOutputDigest: true},
	{name: "usage-not-reported", validationRow: "usageが取得不可能", wantSucceeded: true, wantOutputDigest: true},
	{name: "cli-version-incompatible", validationRow: "CLI version incompatibility", wantParseRefused: true, wantClass: provider.FailureContract},
}

func readFixture(t *testing.T, providerName, caseName string) []byte {
	t.Helper()
	path := filepath.Join("testdata", providerName+"-"+caseName+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(raw) == 0 {
		t.Fatalf("%s is empty; a zero-byte fixture would make its cell pass for the wrong reason", path)
	}
	return raw
}

func TestNineCaseFixtureMatrixOverThreeAdapters(t *testing.T) {
	adapters := allAdapters()
	cells := 0
	var rows []string
	for _, tc := range fixtureMatrix {
		rows = append(rows, tc.validationRow)
		for _, a := range adapters {
			raw := readFixture(t, a.Name(), tc.name)
			result, failure := provider.ParseOrClassify(a, raw)
			cells++
			if result.Provider != a.Name() {
				t.Fatalf("%s/%s: Result.Provider = %q, want %q", a.Name(), tc.name, result.Provider, a.Name())
			}
			if result.Succeeded != tc.wantSucceeded {
				t.Fatalf("%s/%s: Succeeded = %v, want %v", a.Name(), tc.name, result.Succeeded, tc.wantSucceeded)
			}
			if failure.Class != tc.wantClass {
				t.Fatalf("%s/%s: FailureClass = %q, want %q", a.Name(), tc.name, failure.Class, tc.wantClass)
			}
			if failure.Retryable != tc.wantRetryable {
				t.Fatalf("%s/%s: Retryable = %v, want %v", a.Name(), tc.name, failure.Retryable, tc.wantRetryable)
			}
			if failure.Ambiguous != tc.wantAmbiguous {
				t.Fatalf("%s/%s: Ambiguous = %v, want %v", a.Name(), tc.name, failure.Ambiguous, tc.wantAmbiguous)
			}
			if result.UsageReported != tc.wantUsageReported {
				t.Fatalf("%s/%s: UsageReported = %v, want %v", a.Name(), tc.name, result.UsageReported, tc.wantUsageReported)
			}
			if (result.OutputDigest != "") != tc.wantOutputDigest {
				t.Fatalf("%s/%s: OutputDigest = %q, want present=%v", a.Name(), tc.name, result.OutputDigest, tc.wantOutputDigest)
			}
			if tc.wantOutputDigest && !strings.HasPrefix(result.OutputDigest, "sha256:") {
				t.Fatalf("%s/%s: OutputDigest %q is not a digest", a.Name(), tc.name, result.OutputDigest)
			}
			// Whatever the case, no provider text ever reaches the Failure
			// message beyond the bounded, redaction-checked path.
			if len(failure.Message) > 256 {
				t.Fatalf("%s/%s: failure message is %d bytes; safeMessage caps it at 256", a.Name(), tc.name, len(failure.Message))
			}
			_, parseErr := a.Parse(raw)
			if (parseErr != nil) != tc.wantParseRefused {
				t.Fatalf("%s/%s: Parse error = %v, want refused=%v", a.Name(), tc.name, parseErr, tc.wantParseRefused)
			}
		}
	}
	if want := len(fixtureMatrix) * len(adapters); cells != want {
		t.Fatalf("drove %d cells, want %d", cells, want)
	}
	t.Logf("one table, %d cases x %d adapters = %d cells; validation.md rows covered: %v", len(fixtureMatrix), len(adapters), cells, rows)
}

// TestExactlyOneTableDrivesTheFixtureMatrix is A5's "report how many distinct
// table drivers exist" half. Three near-identical copies would satisfy every
// assertion above while defeating the purpose, so the number of range
// statements over the matrix is itself asserted to be one.
func TestExactlyOneTableDrivesTheFixtureMatrix(t *testing.T) {
	files := parseProviderPackage(t)
	declarations, ranges := 0, 0
	for _, f := range files {
		ast.Inspect(f.file, func(n ast.Node) bool {
			if value, isValue := n.(*ast.ValueSpec); isValue {
				for _, name := range value.Names {
					if name.Name == "fixtureMatrix" {
						declarations++
					}
				}
				return true
			}
			rangeStmt, isRange := n.(*ast.RangeStmt)
			if !isRange {
				return true
			}
			if ident, isIdent := rangeStmt.X.(*ast.Ident); isIdent && ident.Name == "fixtureMatrix" {
				ranges++
			}
			return true
		})
	}
	if declarations != 1 {
		t.Fatalf("fixtureMatrix is declared %d times, want exactly 1", declarations)
	}
	if ranges != 1 {
		t.Fatalf("%d range statements drive fixtureMatrix, want exactly 1: a second copy would let a change to build or parseFixture reach fewer than three adapters without failing", ranges)
	}
	t.Logf("distinct table drivers over the nine-case matrix = %d (declarations=%d)", ranges, declarations)
}

// TestUsageReportedAndUsageNotReportedAreDistinguishable is A6. The chosen
// representation is one additive boolean on Result, set by parseFixture from
// a pointer-typed fixture usage field. It changes no existing assertion about
// Result.Usage: Usage still carries exactly the counts the fixture declared,
// and a fixture with no usage object still yields the Usage zero value.
func TestUsageReportedAndUsageNotReportedAreDistinguishable(t *testing.T) {
	for _, a := range allAdapters() {
		reported, failure := provider.ParseOrClassify(a, readFixture(t, a.Name(), "usage-reported"))
		absent, absentFailure := provider.ParseOrClassify(a, readFixture(t, a.Name(), "usage-not-reported"))
		if failure.Class != "" || absentFailure.Class != "" {
			t.Fatalf("%s: both usage cases must succeed, got %q and %q", a.Name(), failure.Class, absentFailure.Class)
		}
		if reported.Usage != absent.Usage {
			t.Fatalf("%s: the two usage cases must have identical Usage values for the distinction to be about reporting; got %#v and %#v", a.Name(), reported.Usage, absent.Usage)
		}
		if !reported.UsageReported {
			t.Fatalf("%s: a usage object present with zero counts must read as reported", a.Name())
		}
		if absent.UsageReported {
			t.Fatalf("%s: an absent usage object must not read as reported", a.Name())
		}
		if reported == absent {
			t.Fatalf("%s: a caller cannot tell the two Results apart", a.Name())
		}
		// The existing assertion about Result.Usage still holds: a fixture
		// that declares no total_tokens yields zero, as it did before.
		if reported.Usage.TotalTokens != 0 {
			t.Fatalf("%s: usage-reported fixture declares zero counts, got total=%d", a.Name(), reported.Usage.TotalTokens)
		}
	}
}

// TestTimeoutAndCancelAreConditionsOfTheInvocationNotShapesOfAnOutput is the
// validation.md row that has no fixture file, driven through the two
// functions that actually see it.
func TestTimeoutAndCancelAreConditionsOfTheInvocationNotShapesOfAnOutput(t *testing.T) {
	for _, a := range allAdapters() {
		timeout := a.NormalizeError(context.DeadlineExceeded)
		if timeout.Class != provider.FailureTimeout || !timeout.Retryable || !timeout.Ambiguous {
			t.Fatalf("%s: timeout normalised to %#v; a timeout is an unknown result, so it is retryable and ambiguous", a.Name(), timeout)
		}
		cancelled := a.NormalizeError(context.Canceled)
		if cancelled.Class != provider.FailureCancelled || cancelled.Retryable || cancelled.Ambiguous {
			t.Fatalf("%s: cancellation normalised to %#v; we stopped it, so it is neither retryable nor ambiguous", a.Name(), cancelled)
		}
	}
	// The same two through the shared classifier, which is what a process
	// supervisor calls, so the semantics cannot diverge per adapter.
	if f := provider.ClassifyError(context.DeadlineExceeded); f.Class != provider.FailureTimeout || !f.Ambiguous {
		t.Fatalf("ClassifyError(deadline) = %#v", f)
	}
	if f := provider.ClassifyError(context.Canceled); f.Class != provider.FailureCancelled || f.Retryable {
		t.Fatalf("ClassifyError(cancel) = %#v", f)
	}
	// And the breaker rows they map to, which is where the difference
	// matters: a timeout counts toward the transport threshold and marks any
	// resulting open ambiguous; a cancellation never counts at all.
	timeoutAction, err := provider.ActionForFailureClass(provider.FailureTimeout)
	if err != nil || timeoutAction != provider.ActionCountTowardWindowedThreshold {
		t.Fatalf("timeout action = %q err=%v", timeoutAction, err)
	}
	cancelAction, err := provider.ActionForFailureClass(provider.FailureCancelled)
	if err != nil || cancelAction != provider.ActionNeitherCountsNorOpens {
		t.Fatalf("cancel action = %q err=%v", cancelAction, err)
	}
}

func TestNoFixtureFileExistsForTimeoutOrCancel(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("testdata is empty")
	}
	for _, entry := range entries {
		name := strings.ToLower(entry.Name())
		if strings.Contains(name, "timeout") || strings.Contains(name, "cancel") {
			t.Fatalf("%s exists; a timeout and a cancellation are conditions of the invocation, not shapes of an output, and are driven through ClassifyError and NormalizeError instead", entry.Name())
		}
	}
	t.Logf("testdata holds %d entries and none of them is a timeout or cancel fixture", len(entries))
}
