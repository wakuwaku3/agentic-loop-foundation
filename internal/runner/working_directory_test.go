package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

// V2-077: the working directory an Invocation declares must actually become
// the child process's working directory.
//
// Every assertion below is either a refusal (which starts no process, needs
// no clock and no filesystem race) or a child that reports its own directory
// and exits. The confined assertions reuse this package's existing bounded
// marker-file deadline poll (waitForConfinementMarker) rather than
// introducing a second waiting idiom. There is no fixed sleep, no
// wall-clock timer and no goroutine anywhere in this file except the one
// borrowed shape the package already uses to read a child's completion, and
// every measured duration is logged as an observation rather than asserted
// as a threshold.

// realDir resolves dir through any symlink so that the result satisfies all
// five of the fail-closed properties on its own (the process temporary
// directory a t.TempDir() sits under is a symlink on some platforms).
func realDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", dir, err)
	}
	return filepath.Clean(resolved)
}

// pwdReportingArgv returns an argv for a harmless child (a POSIX shell, never
// a Provider CLI) that writes its own physical working directory to out and
// exits. "pwd -P" is used rather than the default logical pwd on purpose: a
// chdir performed by exec's own Cmd.Dir does not rewrite the inherited PWD
// environment variable, so only the physical form is a measurement of the
// child's real directory rather than of what its parent's environment
// claimed.
func pwdReportingArgv(out string) []string {
	return []string{resolveTool("sh"), "-c", `pwd -P > "$1"`, "pwd-report", out}
}

func readTrimmed(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}

// --- A3: ProcessSupervisor gains exactly one field, and assigns it. ---

// TestProcessSupervisorDirFieldIsAdditiveAndDocumented pins the shape of the
// change rather than its effect: exactly one field was added, it is named
// Dir, it sits alongside the three fields whose additive contract it copies,
// and the type's own doc comment documents it in the same terms.
func TestProcessSupervisorDirFieldIsAdditiveAndDocumented(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "supervisor.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var fields []string
	var doc string
	ast.Inspect(file, func(n ast.Node) bool {
		decl, ok := n.(*ast.GenDecl)
		if !ok || decl.Tok != token.TYPE {
			return true
		}
		for _, spec := range decl.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "ProcessSupervisor" {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			if decl.Doc != nil {
				doc = decl.Doc.Text()
			}
			for _, f := range st.Fields.List {
				for _, name := range f.Names {
					fields = append(fields, name.Name)
				}
			}
		}
		return true
	})
	want := []string{"TermGrace", "Env", "Confine", "Stdin", "Stdout", "Dir"}
	if strings.Join(fields, ",") != strings.Join(want, ",") {
		t.Fatalf("ProcessSupervisor fields = %v, want exactly %v (one additive field, Dir)", fields, want)
	}
	for _, phrase := range []string{"Dir is additive", "zero-value-compatible", "Confine is non-nil"} {
		if !strings.Contains(doc, phrase) {
			t.Errorf("ProcessSupervisor doc comment does not document Dir in the terms Env, Stdin and Stdout are documented in: missing %q", phrase)
		}
	}
}

// TestProcessSupervisorZeroDirRunsChildInTheTestProcessOwnDirectory is the
// zero-value compatibility assertion: a zero Dir must produce exactly the
// behaviour the code had before this field existed, which is that the child
// inherits the calling process's own directory.
func TestProcessSupervisorZeroDirRunsChildInTheTestProcessOwnDirectory(t *testing.T) {
	own, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	own = realDir(t, own)
	out := filepath.Join(t.TempDir(), "pwd")
	if err := (ProcessSupervisor{}).Run(context.Background(), pwdReportingArgv(out)); err != nil {
		t.Fatalf("zero-value ProcessSupervisor.Run: %v", err)
	}
	if got := readTrimmed(t, out); got != own {
		t.Fatalf("child of a zero-Dir supervisor reported %q, want the test process's own directory %q", got, own)
	}
	t.Logf("execution fact: zero Dir leaves the child in the calling process's own directory (%s)", own)
}

// TestProcessSupervisorDirMakesUnconfinedChildRunThere is the positive
// assertion for the unconfined path.
func TestProcessSupervisorDirMakesUnconfinedChildRunThere(t *testing.T) {
	dir := realDir(t, t.TempDir())
	out := filepath.Join(t.TempDir(), "pwd")
	if err := (ProcessSupervisor{Dir: dir}).Run(context.Background(), pwdReportingArgv(out)); err != nil {
		t.Fatalf("ProcessSupervisor.Run with Dir=%s: %v", dir, err)
	}
	if got := readTrimmed(t, out); got != dir {
		t.Fatalf("child reported directory %q, want exactly the declared Dir %q", got, dir)
	}
	t.Logf("execution fact: a non-empty Dir with Confine nil makes the child report exactly that directory (%s)", dir)
}

// --- A5: with confinement on, the directory is never expressed as cmd.Dir. ---

// cmdDirAssignments returns, for one file of this package, the byte offsets of
// every assignment whose left-hand side is the selector "cmd.Dir".
func cmdDirAssignments(t *testing.T, filename string) (*token.FileSet, *ast.File, []byte, []token.Pos) {
	t.Helper()
	src, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []token.Pos
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assign.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Dir" {
				continue
			}
			ident, ok := sel.X.(*ast.Ident)
			if ok && ident.Name == "cmd" {
				found = append(found, assign.Pos())
			}
		}
		return true
	})
	return fset, file, src, found
}

// TestProcessSupervisorNeverAssignsCmdDirUnderConfinement is a structural
// assertion, not a comment: it finds the single assignment to cmd.Dir in
// supervisor.go and proves it is lexically inside an "if" whose condition
// requires Confine to be nil. Under confinement cmd.Dir could only ever
// chdir before unshare runs, i.e. before either mount pair exists, so it is
// structurally incapable of landing after the read-write remount and is
// therefore never used there.
func TestProcessSupervisorNeverAssignsCmdDirUnderConfinement(t *testing.T) {
	fset, file, src, assignments := cmdDirAssignments(t, "supervisor.go")
	if len(assignments) != 1 {
		t.Fatalf("supervisor.go assigns cmd.Dir %d times, want exactly one guarded assignment", len(assignments))
	}
	at := assignments[0]
	guarded := false
	ast.Inspect(file, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok || ifStmt.Body == nil {
			return true
		}
		if at <= ifStmt.Body.Pos() || at >= ifStmt.Body.End() {
			return true
		}
		lo := fset.Position(ifStmt.Cond.Pos()).Offset
		hi := fset.Position(ifStmt.Cond.End()).Offset
		if strings.Contains(string(src[lo:hi]), "s.Confine == nil") {
			guarded = true
			t.Logf("execution fact: cmd.Dir is assigned only under the condition %q", strings.TrimSpace(string(src[lo:hi])))
		}
		return true
	})
	if !guarded {
		t.Fatal("the assignment to cmd.Dir is not guarded by a condition requiring s.Confine == nil, so a confined run could express its directory through cmd.Dir")
	}
	// The other half of the structural claim: no other file of this package
	// assigns a command's Dir at all, so ProcessSupervisor is the only place
	// a child directory can be assigned.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "supervisor.go" {
			continue
		}
		if _, _, _, other := cmdDirAssignments(t, name); len(other) != 0 {
			t.Errorf("%s also assigns a command's Dir; ProcessSupervisor must be the only place a child directory is assigned", name)
		}
	}
}

// wrapScript returns the shell script text NamespaceConfinement.wrap
// generated, i.e. the argument the wrapped argv hands to "sh -c". The script
// is inspected as text on purpose (A5): reading the source instead would
// prove nothing about what the child actually receives.
func wrapScript(t *testing.T, ws, dir string) string {
	t.Helper()
	wrapped, err := (NamespaceConfinement{Workspace: ws}).wrap([]string{"true"}, dir)
	if err != nil {
		t.Fatalf("wrap(%s, %s): %v", ws, dir, err)
	}
	for i, element := range wrapped {
		if element == "-c" && i+1 < len(wrapped) {
			return wrapped[i+1]
		}
	}
	t.Fatalf("wrapped argv %v carries no -c script", wrapped)
	return ""
}

func scriptLines(script string) []string {
	var out []string
	for _, line := range strings.Split(script, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// TestNamespaceConfinementWrapEmitsOneCdAfterBothMountPairsAndBeforeExec is
// the ordering proof, made against the generated script text.
func TestNamespaceConfinementWrapEmitsOneCdAfterBothMountPairsAndBeforeExec(t *testing.T) {
	parent := realDir(t, t.TempDir())
	ws := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(ws, 0700); err != nil {
		t.Fatal(err)
	}
	lines := scriptLines(wrapScript(t, ws, ws))

	cdIndex := -1
	cdCount := 0
	var mountIndexes []int
	execIndex := -1
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "cd "):
			cdCount++
			cdIndex = i
		case strings.HasPrefix(line, "mount "):
			mountIndexes = append(mountIndexes, i)
		case strings.HasPrefix(line, `exec "$@"`):
			execIndex = i
		}
	}
	if cdCount != 1 {
		t.Fatalf("generated script emits %d cd lines, want exactly one:\n%s", cdCount, strings.Join(lines, "\n"))
	}
	if len(mountIndexes) != 4 {
		t.Fatalf("generated script emits %d mount lines, want the two unchanged pairs (4):\n%s", len(mountIndexes), strings.Join(lines, "\n"))
	}
	if cdIndex < mountIndexes[len(mountIndexes)-1] {
		t.Fatalf("cd (line %d) is not after both mount pairs (last mount at line %d):\n%s", cdIndex, mountIndexes[len(mountIndexes)-1], strings.Join(lines, "\n"))
	}
	if execIndex != cdIndex+1 {
		t.Fatalf("cd is not immediately before exec (cd at %d, exec at %d):\n%s", cdIndex, execIndex, strings.Join(lines, "\n"))
	}
	// Shell-quoted by the same helper the mounts use, not by an ad-hoc
	// second quoting rule.
	if want := "cd " + shQuote(ws); lines[cdIndex] != want {
		t.Fatalf("cd line = %q, want %q (quoted by shQuote, the helper the mounts use)", lines[cdIndex], want)
	}
	// The two mount pairs are byte-identical to the ones a wrap with no
	// directory emits: the directory adds no mount and reorders none.
	before := scriptLines(wrapScript(t, ws, ""))
	var beforeMounts, afterMounts []string
	for _, line := range before {
		if strings.HasPrefix(line, "mount ") {
			beforeMounts = append(beforeMounts, line)
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "mount ") {
			afterMounts = append(afterMounts, line)
		}
	}
	if strings.Join(beforeMounts, "\n") != strings.Join(afterMounts, "\n") {
		t.Fatalf("the mount lines changed when a directory was supplied:\nwithout:\n%s\nwith:\n%s", strings.Join(beforeMounts, "\n"), strings.Join(afterMounts, "\n"))
	}
	// An empty directory keeps the pre-V2-077 script exactly: no cd at all.
	for _, line := range before {
		if strings.HasPrefix(line, "cd ") {
			t.Fatalf("wrap with an empty directory emitted a cd line %q; the zero value must keep the previous script", line)
		}
	}
	t.Logf("execution fact: generated confinement script (with directory) =\n%s", strings.Join(lines, "\n"))
}

// TestNamespaceConfinementWrapRefusesDirectoryOutsideWorkspace proves the
// refusal, including the two cases the design names explicitly: a sibling
// and an ancestor.
func TestNamespaceConfinementWrapRefusesDirectoryOutsideWorkspace(t *testing.T) {
	parent := realDir(t, t.TempDir())
	ws := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(ws, 0700); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"sibling":                     filepath.Join(parent, "sibling"),
		"sibling-sharing-a-prefix":    ws + "-2",
		"ancestor":                    parent,
		"root":                        "/",
		"relative":                    "workspace",
		"non-canonical-traversal":     ws + string(filepath.Separator) + ".." + string(filepath.Separator) + "workspace",
		"non-canonical-doubled-slash": strings.Replace(ws, string(filepath.Separator), string(filepath.Separator)+string(filepath.Separator), 1),
		"non-canonical-trailing":      ws + string(filepath.Separator),
	}
	for name, dir := range cases {
		t.Run(name, func(t *testing.T) {
			wrapped, err := (NamespaceConfinement{Workspace: ws}).wrap([]string{"true"}, dir)
			if err == nil {
				t.Fatalf("wrap accepted %s (%q), which is not the workspace and not beneath it", name, dir)
			}
			if wrapped != nil {
				t.Fatalf("wrap returned a wrapped argv (%v) alongside its error; nothing may be wrapped, so no child can start", wrapped)
			}
		})
	}
	// And the workspace itself, plus a path beneath it, are accepted.
	for name, dir := range map[string]string{"workspace-itself": ws, "beneath-the-workspace": filepath.Join(ws, "sub")} {
		t.Run(name, func(t *testing.T) {
			if _, err := (NamespaceConfinement{Workspace: ws}).wrap([]string{"true"}, dir); err != nil {
				t.Fatalf("wrap refused %s (%q): %v", name, dir, err)
			}
		})
	}
}

// TestConfinedChildRunsInTheWorkspaceAndResolvesRelativePathsFromIt is the
// end-to-end confined assertion. It reuses waitForConfinementMarker, the
// bounded deadline poll this package already uses, and is recorded as
// skipped -- never as a pass -- when this kernel cannot provide the
// namespace (gate rule G7).
func TestConfinedChildRunsInTheWorkspaceAndResolvesRelativePathsFromIt(t *testing.T) {
	if err := (NamespaceConfinement{}).Probe(context.Background()); err != nil {
		t.Skipf("rootless user+mount namespaces are not usable in this environment (%v); skipping, and this run is recorded as skipped and never as a pass", err)
	}
	var uname syscall.Utsname
	kernel := "unknown"
	if err := syscall.Uname(&uname); err == nil {
		kernel = utsnameToString(uname.Release)
	}
	t.Logf("execution fact: kernel = %s, GOOS/GOARCH = %s/%s", kernel, runtime.GOOS, runtime.GOARCH)

	parent := realDir(t, t.TempDir())
	ws := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(ws, 0700); err != nil {
		t.Fatal(err)
	}
	// Every path the child names is relative, so the measurement is of the
	// child's own resolution base rather than of a path the test supplied.
	script := `set +e
pwd -P > cwd
echo inside 2> inside.err > relative-inside.txt
echo $? > inside.exit
echo above 2> above.err > ../relative-above.txt
echo $? > above.exit
touch done
`
	supervisor := ProcessSupervisor{Confine: &NamespaceConfinement{Workspace: ws}, Dir: ws}
	runErr := make(chan error, 1)
	go func() {
		runErr <- supervisor.Run(context.Background(), []string{resolveTool("sh"), "-c", script, "confined-cwd"})
	}()
	waitForConfinementMarker(t, filepath.Join(ws, "done"), 15*time.Second)
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("confined ProcessSupervisor.Run returned an error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("confined ProcessSupervisor.Run did not return after the child finished")
	}

	if got := readTrimmed(t, filepath.Join(ws, "cwd")); got != ws {
		t.Fatalf("confined child reported working directory %q, want the workspace %q", got, ws)
	}
	if got := readTrimmed(t, filepath.Join(ws, "inside.exit")); got != "0" {
		t.Fatalf("relative write inside the workspace failed: exit=%s stderr=%q", got, readTrimmed(t, filepath.Join(ws, "inside.err")))
	}
	if got := readTrimmed(t, filepath.Join(ws, "above.exit")); got == "0" {
		t.Fatal("a relative write ABOVE the workspace succeeded under confinement; the directory was made to work by widening what the child may reach")
	}
	aboveErr := readTrimmed(t, filepath.Join(ws, "above.err"))
	t.Logf("execution fact: confined child cwd = %s; relative upward write refused with stderr %q", ws, aboveErr)
	if _, err := os.Stat(filepath.Join(parent, "relative-above.txt")); !os.IsNotExist(err) {
		t.Fatalf("the upward relative write must never have reached the host filesystem, stat error = %v", err)
	}
}

// TestRejectedOrderingInheritedCwdIsMeasuredNotPredicted is A6. It measures,
// with a positive control, whether the ordering the design REJECTED -- a cwd
// inherited across namespace creation, which is the only thing cmd.Dir can do
// when Confine is non-nil -- lets a relative upward write reach the mount
// that existed before the read-only remount. Both outcomes are admissible:
// the result is recorded either way and the design is not changed on the
// basis of it, because d4's argument is structural.
func TestRejectedOrderingInheritedCwdIsMeasuredNotPredicted(t *testing.T) {
	if err := (NamespaceConfinement{}).Probe(context.Background()); err != nil {
		t.Skipf("rootless user+mount namespaces are not usable in this environment (%v); skipping, and this run is recorded as skipped and never as a pass", err)
	}
	var uname syscall.Utsname
	kernel := "unknown"
	if err := syscall.Uname(&uname); err == nil {
		kernel = utsnameToString(uname.Release)
	}

	parent := realDir(t, t.TempDir())
	ws := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(ws, 0700); err != nil {
		t.Fatal(err)
	}
	script := `set +e
pwd -P > cwd
echo above 2> above.err > ../rejected-above.txt
echo $? > above.exit
touch done
`
	// The rejected ordering, expressed exactly as os/exec would express it:
	// wrap with NO in-namespace cd, and the cwd supplied through the Cmd's
	// own Dir, which chdir's in the forked child before unshare execs and
	// therefore before either mount pair exists.
	wrapped, err := (NamespaceConfinement{Workspace: ws}).wrap([]string{resolveTool("sh"), "-c", script, "rejected-ordering"}, "")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapped[0], wrapped[1:]...)
	cmd.Dir = ws
	if err := cmd.Run(); err != nil {
		t.Logf("observation: the rejected ordering's child exited with %v (the measurement below is read from its own marker files)", err)
	}
	waitForConfinementMarker(t, filepath.Join(ws, "done"), 15*time.Second)

	rejectedCwd := readTrimmed(t, filepath.Join(ws, "cwd"))
	rejectedExit := readTrimmed(t, filepath.Join(ws, "above.exit"))
	rejectedErr := readTrimmed(t, filepath.Join(ws, "above.err"))
	_, hostErr := os.Stat(filepath.Join(parent, "rejected-above.txt"))
	reachedHost := hostErr == nil

	t.Logf("MEASUREMENT (A6, kernel %s): rejected ordering (cmd.Dir inherited across namespace creation): cwd=%q upward-relative-write-exit=%q stderr=%q reached-host-filesystem=%v", kernel, rejectedCwd, rejectedExit, rejectedErr, reachedHost)
	if rejectedExit == "0" || reachedHost {
		t.Logf("MEASURED RESULT: the rejected ordering ACTUALLY PERMITS a relative upward write the seal was meant to stop. The chosen in-namespace cd is therefore not merely the provable ordering but the only safe one.")
	} else {
		t.Logf("MEASURED RESULT: the rejected ordering did NOT permit the upward write on this kernel; it is merely UNPROVABLE, not demonstrably exploitable. The design still chooses the in-namespace cd, because only its after-the-remount property is provable rather than inferred.")
	}

	// Positive control: the chosen ordering, measured on an identical
	// workspace, must refuse the same upward relative write.
	controlParent := realDir(t, t.TempDir())
	controlWS := filepath.Join(controlParent, "workspace")
	if err := os.MkdirAll(controlWS, 0700); err != nil {
		t.Fatal(err)
	}
	controlSupervisor := ProcessSupervisor{Confine: &NamespaceConfinement{Workspace: controlWS}, Dir: controlWS}
	controlErr := make(chan error, 1)
	go func() {
		controlErr <- controlSupervisor.Run(context.Background(), []string{resolveTool("sh"), "-c", script, "chosen-ordering"})
	}()
	waitForConfinementMarker(t, filepath.Join(controlWS, "done"), 15*time.Second)
	select {
	case err := <-controlErr:
		if err != nil {
			t.Fatalf("positive control (chosen ordering) returned an error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("positive control (chosen ordering) did not return after the child finished")
	}
	if got := readTrimmed(t, filepath.Join(controlWS, "cwd")); got != controlWS {
		t.Fatalf("positive control: chosen ordering put the child in %q, want %q", got, controlWS)
	}
	if got := readTrimmed(t, filepath.Join(controlWS, "above.exit")); got == "0" {
		t.Fatalf("positive control: the chosen ordering also permitted the upward relative write, so the measurement above is not informative")
	}
	t.Logf("execution fact: positive control (chosen in-namespace cd) refused the identical upward relative write, so the comparison above is informative")
}

// --- A4: the policy is in SupervisedInvocationRunner, fail-closed on five
// properties, and it refuses before LoadPreflightRecord and before
// Ledger.Reserve. ---

func invocationWithWorkingDirectory(dir string) provider.Invocation {
	return provider.Invocation{
		Argv:             []string{"claude", "--print", "--output-format", "json", "--no-session-persistence"},
		WorkingDirectory: dir,
	}
}

// TestSupervisedInvocationRunnerRefusesUnusableWorkingDirectory is the
// fail-closed matrix. Each case is its own subtest, each asserts the same
// sentinel with a distinct wrapped reason, and each asserts that no process
// was started AND that no ledger file was ever created -- which is how the
// "before Ledger.Reserve" ordering is shown: Reserve persists a reservation
// to disk before anything may execute, so an absent ledger file is proof
// that Reserve was never reached.
func TestSupervisedInvocationRunnerRefusesUnusableWorkingDirectory(t *testing.T) {
	type buildCase struct {
		name string
		dir  func(t *testing.T, scratch string) string
	}
	cases := []buildCase{
		{"empty", func(*testing.T, string) string { return "" }},
		{"relative", func(*testing.T, string) string { return filepath.Join("relative", "workspace") }},
		{"non-canonical-traversal-segment", func(t *testing.T, scratch string) string {
			// Built by concatenation rather than filepath.Join, because Join
			// canonicalises and would erase the very property under test.
			return scratch + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(scratch)
		}},
		{"non-canonical-trailing-separator", func(t *testing.T, scratch string) string {
			return scratch + string(filepath.Separator)
		}},
		{"non-canonical-doubled-separator", func(t *testing.T, scratch string) string {
			return strings.Replace(scratch, string(filepath.Separator), string(filepath.Separator)+string(filepath.Separator), 1)
		}},
		{"does-not-exist", func(t *testing.T, scratch string) string {
			return filepath.Join(scratch, "absent")
		}},
		{"exists-but-is-a-regular-file", func(t *testing.T, scratch string) string {
			path := filepath.Join(scratch, "regular")
			if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{"symlink-to-a-valid-directory", func(t *testing.T, scratch string) string {
			target := filepath.Join(scratch, "target")
			if err := os.MkdirAll(target, 0700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(scratch, "link")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
	}
	reasons := map[string]string{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newRunnerFixture(t)
			scratch := realDir(t, t.TempDir())
			dir := c.dir(t, scratch)
			_, err := fx.runner.Run(context.Background(), invocationWithWorkingDirectory(dir))
			if !errors.Is(err, ErrInvocationWorkingDirectoryUnusable) {
				t.Fatalf("want ErrInvocationWorkingDirectoryUnusable for %s (%q), got %v", c.name, dir, err)
			}
			fx.assertNoProcessStarted(t)
			if _, statErr := os.Stat(fx.ledgerPath); !os.IsNotExist(statErr) {
				t.Fatalf("a ledger file exists after the refusal (stat error = %v), so Ledger.Reserve was reached before the refusal", statErr)
			}
			reason := strings.TrimPrefix(err.Error(), ErrInvocationWorkingDirectoryUnusable.Error()+": ")
			if dir != "" {
				reason = strings.ReplaceAll(reason, dir, "<dir>")
			}
			reasons[c.name] = reason
			t.Logf("execution fact: %s refused with reason %q, no process started, no ledger file created", c.name, reasons[c.name])
		})
	}
	// The five properties must be distinguishable from the outside: the
	// three non-canonical cases legitimately share one reason (they are one
	// property), every other case has its own.
	distinct := map[string]string{}
	for name, reason := range reasons {
		if strings.HasPrefix(name, "non-canonical") {
			continue
		}
		if other, clash := distinct[reason]; clash {
			t.Errorf("%s and %s report the identical reason %q; each property must wrap its own", name, other, reason)
		}
		distinct[reason] = name
	}
	if len(reasons) != 8 {
		t.Fatalf("recorded %d refusal reasons, want 8", len(reasons))
	}
}

// TestSupervisedInvocationRunnerRefusesConfinedDirectoryOutsideWorkspace is
// the sixth clause: with Confine set, a directory outside the confined
// workspace is refused rather than repaired, and the confinement is never
// relaxed to make it take effect.
func TestSupervisedInvocationRunnerRefusesConfinedDirectoryOutsideWorkspace(t *testing.T) {
	fx := newRunnerFixture(t)
	parent := realDir(t, t.TempDir())
	ws := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside")
	for _, dir := range []string{ws, outside} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	fx.runner.Supervisor.Confine = &NamespaceConfinement{Workspace: ws}
	_, err := fx.runner.Run(context.Background(), invocationWithWorkingDirectory(outside))
	if !errors.Is(err, ErrInvocationWorkingDirectoryUnusable) {
		t.Fatalf("want ErrInvocationWorkingDirectoryUnusable for a confined directory outside the workspace, got %v", err)
	}
	fx.assertNoProcessStarted(t)
	if _, statErr := os.Stat(fx.ledgerPath); !os.IsNotExist(statErr) {
		t.Fatalf("a ledger file exists after the refusal (stat error = %v)", statErr)
	}
}

// TestSupervisedInvocationRunnerWorkingDirectoryRefusalPrecedesPreflightLoad
// shows the ordering against LoadPreflightRecord directly: the record path
// names a file that does not exist, so if the load ran first its error is
// what would come back. The working-directory reason coming back instead is
// the proof.
func TestSupervisedInvocationRunnerWorkingDirectoryRefusalPrecedesPreflightLoad(t *testing.T) {
	fx := newRunnerFixture(t)
	fx.runner.RecordPath = filepath.Join(t.TempDir(), "no-such-preflight-record.json")
	_, err := fx.runner.Run(context.Background(), invocationWithWorkingDirectory(""))
	if !errors.Is(err, ErrInvocationWorkingDirectoryUnusable) {
		t.Fatalf("want the working-directory refusal to come back ahead of the preflight load error, got %v", err)
	}
	fx.assertNoProcessStarted(t)
	if _, statErr := os.Stat(fx.ledgerPath); !os.IsNotExist(statErr) {
		t.Fatalf("a ledger file exists after the refusal (stat error = %v)", statErr)
	}
}

// writeCwdReportingExecutable writes a harmless fake "claude" -- a POSIX
// shell script, never the real CLI -- that records its own physical working
// directory and then emits the minimal success JSON the runner projects.
func writeCwdReportingExecutable(t *testing.T, path, cwdPath, markerPath string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"touch " + shQuoteTest(markerPath) + "\n" +
		"pwd -P > " + shQuoteTest(cwdPath) + "\n" +
		"cat <<'JSON'\n" +
		`{"type":"result","subtype":"success","is_error":false,"session_id":"workdir-fixture-session","result":"fixture output","total_cost_usd":0.01,"duration_ms":1,"duration_api_ms":1,"num_turns":1,"usage":{"input_tokens":1,"output_tokens":1}}` + "\n" +
		"JSON\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

// TestSupervisedInvocationRunnerRunsTheChildInTheDeclaredWorkingDirectory is
// the happy path, and it is also the decisive local record of what actually
// changed for the child: a real child process (a POSIX shell standing in for
// the Provider CLI, so no Provider usage is consumed and no credential is
// needed) reports its own physical working directory, and that directory is
// compared value-to-value against Invocation.WorkingDirectory. Because
// SupervisedInvocationRunner's only route to a child is sup.Dir, the child's
// own report IS the assertion that sup.Dir equals Invocation.WorkingDirectory
// exactly.
func TestSupervisedInvocationRunnerRunsTheChildInTheDeclaredWorkingDirectory(t *testing.T) {
	scratch := realDir(t, t.TempDir())
	workspace := filepath.Join(scratch, "workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	execPath := filepath.Join(scratch, "claude")
	cwdPath := filepath.Join(scratch, "child.cwd")
	marker := filepath.Join(scratch, "marker")
	writeCwdReportingExecutable(t, execPath, cwdPath, marker)
	recordPath := writeFixturePreflightRecord(t, mustRepoRoot(t), scratch, "V2-999", execPath, fixtureLimits{MaxInvocations: 4, MaxTotalCostUSD: 10, WorstCaseReservationUSD: 1})
	log, err := NewBoundedLog(scratch, "workdir-execution", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	r := SupervisedInvocationRunner{
		Supervisor: ProcessSupervisor{TermGrace: 2 * time.Second},
		Log:        log,
		Ledger:     &CostLedger{Path: filepath.Join(scratch, "ledger.json"), Provider: "claude", TaskID: "V2-999"},
		RepoRoot:   mustRepoRoot(t),
		RecordPath: recordPath,
		Purpose:    "V2-077-declared-working-directory",
	}
	inv := invocationWithWorkingDirectory(workspace)

	// The runner's own directory is deliberately NOT the workspace, so a
	// child that reported the runner's directory would be visibly wrong
	// rather than accidentally right.
	own, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	own = realDir(t, own)
	if own == workspace {
		t.Fatalf("the test process's own directory equals the workspace (%s), so this measurement could not distinguish them", own)
	}

	projected, err := r.Run(context.Background(), inv)
	if err != nil {
		t.Fatalf("happy path Run: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(projected, &parsed); err != nil {
		t.Fatalf("projection is not JSON: %v", err)
	}
	if parsed["subtype"] != "success" {
		t.Fatalf("projection subtype = %v, want success (projection = %v)", parsed["subtype"], parsed)
	}
	childCWD := readTrimmed(t, cwdPath)
	if childCWD != inv.WorkingDirectory {
		t.Fatalf("the real child ran in %q; Invocation.WorkingDirectory is %q, so the declared directory did not reach the child", childCWD, inv.WorkingDirectory)
	}
	if childCWD == own {
		t.Fatalf("the child ran in the runner's own directory %q, which is the defect V2-077 removes", own)
	}
	t.Logf("execution fact (A11, decided locally with a real child and no Provider CLI): declared WorkingDirectory = %s; child's own reported cwd = %s; runner's own directory = %s; they are equal to the declared value and different from the runner's", inv.WorkingDirectory, childCWD, own)
}

// --- A7: the directory flags stay, and the double expression is proven
// harmless rather than assumed so. ---

// TestSupervisedInvocationRunnerRefusesWhenTheDirectoryFlagDisagrees drives a
// hand-built Invocation whose -C / --dir argument is not the declared
// working directory. The two are one string by construction today, so a
// disagreement means something rebuilt one of them: it is refused with the
// same sentinel rather than resolved in either direction.
func TestSupervisedInvocationRunnerRefusesWhenTheDirectoryFlagDisagrees(t *testing.T) {
	for _, flag := range []string{"-C", "--dir"} {
		t.Run(flag, func(t *testing.T) {
			fx := newRunnerFixture(t)
			scratch := realDir(t, t.TempDir())
			declared := filepath.Join(scratch, "declared")
			other := filepath.Join(scratch, "other")
			for _, dir := range []string{declared, other} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			inv := provider.Invocation{
				Argv:             []string{"claude", "exec", flag, other},
				WorkingDirectory: declared,
			}
			_, err := fx.runner.Run(context.Background(), inv)
			if !errors.Is(err, ErrInvocationWorkingDirectoryUnusable) {
				t.Fatalf("want ErrInvocationWorkingDirectoryUnusable when %s carries %q while WorkingDirectory is %q, got %v", flag, other, declared, err)
			}
			fx.assertNoProcessStarted(t)
			if _, statErr := os.Stat(fx.ledgerPath); !os.IsNotExist(statErr) {
				t.Fatalf("a ledger file exists after the refusal (stat error = %v)", statErr)
			}
			// And the agreeing case is accepted by the same check: it is
			// driven far enough to prove the check itself is what refused
			// above, by reaching a later, different refusal reason.
			agreeing := provider.Invocation{
				Argv:             []string{"claude", "exec", flag, declared},
				WorkingDirectory: declared,
			}
			if _, err := fx.runner.Run(context.Background(), agreeing); errors.Is(err, ErrInvocationWorkingDirectoryUnusable) {
				t.Fatalf("the agreeing case was also refused as an unusable working directory: %v", err)
			}
		})
	}
}

// TestDirectoryFlagArgumentFindsEveryFlagTheAdaptersEmit pins the closed list
// the runner compares against. It is a declaration table, not a measurement
// of a CLI: the measurement of which flags exist lives in
// internal/provider/adapters_test.go and in
// docs/operations/provider-adapters.md.
func TestDirectoryFlagArgumentFindsEveryFlagTheAdaptersEmit(t *testing.T) {
	for _, flag := range providerDirectoryFlags {
		got, value, ok := directoryFlagArgument([]string{"cli", "sub", flag, "/workspace", "tail"})
		if !ok || got != flag || value != "/workspace" {
			t.Fatalf("directoryFlagArgument did not read %s: got (%q, %q, %v)", flag, got, value, ok)
		}
	}
	if _, _, ok := directoryFlagArgument([]string{"claude", "--print", "--output-format", "json", "--no-session-persistence"}); ok {
		t.Fatal("claude's argv carries no directory flag, so directoryFlagArgument must report none")
	}
	// A trailing flag with no argument is a disagreement, not a pass.
	if flag, value, ok := directoryFlagArgument([]string{"cli", "-C"}); !ok || flag != "-C" || value != "" {
		t.Fatalf("a trailing directory flag must be reported with an empty argument, got (%q, %q, %v)", flag, value, ok)
	}
}

// --- A9 (V2-077) / A6 (V2-078): the class of "a field the Invocation
// declares and Run does not consume" is closed mechanically. ---

// TestInvocationEnvironmentStaysUnconsumedByTheRunner keeps its exact name
// across two tasks on purpose: nothing else would tell a future reader that
// this is where a recorded defect became a removal.
//
// HISTORICAL MEASUREMENT, 2026-08-25 (V2-077): this test recorded, as an
// executable measurement rather than as prose, the same-shaped defect V2-077
// deliberately did NOT fix -- SupervisedInvocationRunner never read
// Invocation.Environment. It built the child's environment itself from the
// approved record's base names, so a Secret Broker grant merged onto the
// Invocation did not reach a real child. The defect was latent because every
// live exercise so far ran with granted_names empty. Fixing it was left to a
// task with its own credential-isolation acceptance, recorded as V2-078.
//
// CURRENT MEASUREMENT, 2026-08-26 (V2-078): V2-078 chose dp-v2-078 route (b)
// and DELETED the field, together with Grant.Apply and ProviderClient.Grant,
// because the child's environment is already an exclusive total description
// rebuilt from the approved record and handed to a supervisor that REPLACES
// the parent environment -- so deleting the field makes single authority a
// property of the type system instead of a fact about what Run happens to
// read.
//
// That deletion is exactly why this test's INTENT had to change rather than
// its name: V2-077's assertion was the ABSENCE of a read, and a read of a
// field that does not exist cannot compile, so the old assertion could no
// longer fail and a guard that cannot fail is not a guard. It is generalised
// here from "Run does not read Environment" into the class guard "Invocation
// declares no field that Run does not consume", so the next field of this
// shape turns this test red the day it is added rather than three live
// exercises later. Deleting this test instead would let the same defect class
// return silently, which is the one outcome both V2-077 and V2-078 exist to
// prevent.
func TestInvocationEnvironmentStaysUnconsumedByTheRunner(t *testing.T) {
	// (1) By reflection: provider.Invocation's exported field set is
	// exactly {Argv, Stdin, WorkingDirectory}, compared as a whole sorted
	// set so an ADDED field fails just as loudly as a missing one.
	wantFields := []string{"Argv", "Stdin", "WorkingDirectory"}
	typ := reflect.TypeOf(provider.Invocation{})
	var gotFields []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.IsExported() {
			gotFields = append(gotFields, f.Name)
		}
	}
	sort.Strings(gotFields)
	if !slices.Equal(gotFields, wantFields) {
		t.Fatalf("provider.Invocation's exported field set is %v, want exactly %v. An Invocation field that SupervisedInvocationRunner.Run does not consume is the defect class this guard closes (V2-077's WorkingDirectory, V2-078's Environment): CONSUME the new field in Run, or update this record deliberately with the measurement that says why it is consumed elsewhere. Do NOT delete this test.", gotFields, wantFields)
	}

	// (2) The V2-077 AST scan of SupervisedInvocationRunner.Run, kept and
	// strengthened: Run must read EVERY field in (1), and must read no
	// selector on inv outside that set. This is the half that can fail
	// while (1) holds, and vice versa.
	src, err := os.ReadFile("provider.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "provider.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var reads []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Run" || fn.Recv == nil {
			return true
		}
		if !strings.Contains(fmt.Sprintf("%T", fn.Recv.List[0].Type), "Ident") {
			return true
		}
		ident, _ := fn.Recv.List[0].Type.(*ast.Ident)
		if ident == nil || ident.Name != "SupervisedInvocationRunner" {
			return true
		}
		ast.Inspect(fn, func(inner ast.Node) bool {
			sel, ok := inner.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			base, ok := sel.X.(*ast.Ident)
			if !ok || base.Name != "inv" {
				return true
			}
			reads = append(reads, sel.Sel.Name)
			return true
		})
		return true
	})
	seen := map[string]bool{}
	for _, r := range reads {
		seen[r] = true
	}
	declared := map[string]bool{}
	for _, f := range wantFields {
		declared[f] = true
		if !seen[f] {
			t.Fatalf("provider.Invocation declares %s but SupervisedInvocationRunner.Run does not read it; Run reads %v. CONSUME the field in Run, or update this record deliberately with the measurement that says why it is consumed elsewhere. Do NOT delete this test.", f, reads)
		}
	}
	for _, r := range reads {
		if !declared[r] {
			t.Fatalf("SupervisedInvocationRunner.Run reads inv.%s, which is not in provider.Invocation's declared exported field set %v; Run reads %v. Reconcile the two deliberately -- either the field belongs in the set or the read does not belong in Run. Do NOT delete this test.", r, wantFields, reads)
		}
	}

	// (3) Environment specifically is absent from (1). Kept as its own
	// assertion because it is the one field whose return this guard exists
	// to make loud, and because its failure message carries the
	// instruction that says where a credential path actually belongs.
	if slices.Contains(gotFields, "Environment") {
		t.Fatalf("provider.Invocation declares an Environment field again. A future credential path does NOT belong on the Invocation: dp-v2-078 d7's sanctioned shape is SupervisedInvocationRunner leasing exactly the names the approved provider-preflight record's environment.granted_names declares -- names from the digest-bound record, values from the Secret Broker, never a name the record did not authorise. Re-adding this field concedes a second authority over what a child may reach, which is the defect V2-078 removed. Do NOT delete this test.")
	}

	// (4) By reflection: the two things that only existed to feed the
	// deleted field are gone, so a revert is loud rather than quiet.
	if _, ok := reflect.TypeOf(&Grant{}).MethodByName("Apply"); ok {
		t.Fatal("runner.Grant has a method named Apply again. Grant.Apply was the ONLY writer of provider.Invocation.Environment in the tree and wrote a field no child ever received; restoring it restores the pretence of a delivery channel. dp-v2-078 d7 names the sanctioned shape instead. Do NOT delete this test.")
	}
	if _, ok := reflect.TypeOf(ProviderClient{}).FieldByName("Grant"); ok {
		t.Fatal("runner.ProviderClient has a field named Grant again. It was assigned by no file in the tree, and the merge it guarded reached only a test fake. dp-v2-078 d7 names the sanctioned shape instead. Do NOT delete this test.")
	}

	t.Logf("execution fact (V2-078 A6, class guard): provider.Invocation's exported field set is exactly %v; SupervisedInvocationRunner.Run reads %v from the Invocation -- every declared field and nothing else; Grant has no Apply method and ProviderClient has no Grant field. The environment a child receives therefore has exactly one authority, the approved provider-preflight record, with no second field able to carry one.", gotFields, reads)
}

// --- V2-078 A10: nothing about what a real child receives changes, and the
// single authority over it is asserted rather than assumed. ---

// TestChildEnvironmentIsExactlyTheApprovedRecordBaseNames measures the claim
// V2-078's deletion rests on: the approved provider-preflight record is
// already the COMPLETE and EXCLUSIVE description of the environment a child
// receives, so removing provider.Invocation's Environment field removed the
// only field that suggested otherwise and changed the bytes a real child
// receives not at all.
//
// The measurement is in two halves, both deterministic and neither timed:
//
//	(1) buildEnvironmentFromBaseNames -- the one function Run uses to build
//	    the child's environment -- returns a set of names exactly equal to
//	    the record's environment.base_names, no more and no fewer;
//	(2) a real child run through ProcessSupervisor with that Env reports its
//	    own environment, and the names it reports are exactly that same set.
//	    Half (2) is what makes half (1) a fact about a child rather than a
//	    fact about a slice: ProcessSupervisor assigns cmd.Env, which REPLACES
//	    the parent environment rather than extending it, so anything the
//	    parent has and the record does not declare must be absent from the
//	    child's own report.
//
// The child is /usr/bin/env or equivalent -- never a Provider CLI -- and it
// prints NAMES only. No value is printed, compared or logged.
func TestChildEnvironmentIsExactlyTheApprovedRecordBaseNames(t *testing.T) {
	scratch := realDir(t, t.TempDir())
	execPath := filepath.Join(scratch, "claude")
	marker := filepath.Join(scratch, "marker")
	writeFakeExecutable(t, execPath, marker)
	recordPath := writeFixturePreflightRecord(t, mustRepoRoot(t), scratch, "V2-993", execPath, fixtureLimits{MaxInvocations: 4, MaxTotalCostUSD: 10, WorstCaseReservationUSD: 1})
	record, err := LoadPreflightRecord(mustRepoRoot(t), recordPath)
	if err != nil {
		t.Fatalf("load preflight record: %v", err)
	}
	if len(record.EnvironmentBaseNames) == 0 {
		t.Fatal("the fixture record declares no base names, so this measurement would be vacuous")
	}

	// (1) Set equality between the built environment and the record.
	env, err := buildEnvironmentFromBaseNames(record.EnvironmentBaseNames)
	if err != nil {
		t.Fatalf("build environment: %v", err)
	}
	built := map[string]bool{}
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("built environment entry is not NAME=value shaped")
		}
		built[name] = true
	}
	declared := map[string]bool{}
	for _, n := range record.EnvironmentBaseNames {
		declared[n] = true
	}
	if len(built) != len(declared) {
		t.Fatalf("built environment names = %d, record base names = %d; the two must be set-equal", len(built), len(declared))
	}
	for n := range declared {
		if !built[n] {
			t.Fatalf("record declares base name %s but the built environment does not carry it", n)
		}
	}
	for n := range built {
		if !declared[n] {
			t.Fatalf("built environment carries %s, which the record does not declare", n)
		}
	}

	// (2) A real child, with that Env, reports exactly those names.
	var stdout bytes.Buffer
	sup := ProcessSupervisor{TermGrace: 2 * time.Second, Env: env, Stdout: &stdout}
	if err := sup.Run(context.Background(), []string{resolveTool("env")}); err != nil {
		t.Fatalf("child that reports its own environment: %v", err)
	}
	reported := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line == "" {
			continue
		}
		name, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		reported[name] = true
	}
	if len(reported) == 0 {
		t.Fatal("the child reported no environment names at all, so this measurement would be vacuous")
	}
	for n := range declared {
		if !reported[n] {
			t.Fatalf("the child did not receive declared base name %s; it reported %v", n, sortedNames(reported))
		}
	}
	for n := range reported {
		if !declared[n] {
			t.Fatalf("the child received %s, which the approved record does not declare; cmd.Env REPLACES the parent environment, so this would mean a second authority exists. Child reported %v, record declares %v", n, sortedNames(reported), sortedNames(declared))
		}
	}
	t.Logf("execution fact (V2-078 A10): the environment a child receives is set-equal to the approved record's environment.base_names, measured as NAMES only (%v) with no value read, compared or logged. ProcessSupervisor assigns cmd.Env, which replaces rather than extends the parent environment, so the record is the complete and exclusive description of what the child can read.", sortedNames(reported))
}

// sortedNames renders a name set deterministically for a failure message. It
// exists so no assertion above depends on map iteration order.
func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
