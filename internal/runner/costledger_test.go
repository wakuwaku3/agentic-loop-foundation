package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// mustRepoRoot resolves the real repository root from internal/runner's own
// directory. Using the real root (rather than a synthetic fixture tree)
// means these tests exercise the real contracts/schemas/provider-preflight.json
// schema and the real .agents/v2/packets/V2-017-work-order.json file as an
// approval subject, without ever writing to any prohibited path: every
// fixture record below lives in a t.TempDir(), and only ever *reads* the
// real repository tree.
func mustRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("computed root %s does not look like the repository root: %v", root, err)
	}
	return root
}

func shQuoteTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeFakeExecutable writes an executable shell script at path that, when
// run, touches markerPath and prints a minimal, valid real-CLI-shaped
// success JSON to stdout. Its only purpose is to make "a process WAS
// started" or "a process was NEVER started" an observable, checkable fact
// (marker file present/absent) for the fail-closed tests below; it is never
// a stand-in for the real CLI's response shape (provider_live_test.go's
// live suite exercises the real binary for that).
func writeFakeExecutable(t *testing.T, path, markerPath string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"touch " + shQuoteTest(markerPath) + "\n" +
		"cat <<'JSON'\n" +
		`{"type":"result","subtype":"success","is_error":false,"session_id":"fixture-session","result":"fixture output","total_cost_usd":0.01,"duration_ms":1,"duration_api_ms":1,"num_turns":1,"usage":{"input_tokens":1,"output_tokens":1}}` + "\n" +
		"JSON\n"
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

type fixtureLimits struct {
	MaxInvocations          int
	MaxTotalCostUSD         float64
	WorstCaseReservationUSD float64
}

// writeFixturePreflightRecord writes a schema-valid provider-preflight
// record at dir/<taskID>-fixture.json, bound (via approval.subject_path) to
// the real, already-existing .agents/v2/packets/V2-017-work-order.json file
// so LoadPreflightRecord's subject-digest check passes against real bytes
// without this test ever writing to a prohibited path.
func writeFixturePreflightRecord(t *testing.T, repoRoot, dir, taskID, executablePath string, limits fixtureLimits) string {
	t.Helper()
	return writeFixturePreflightRecordWithEnvironment(t, repoRoot, dir, taskID, executablePath, limits, "claude", []string{"HOME", "PATH"}, []string{})
}

// writeFixturePreflightRecordWithEnvironment is the same helper with the two
// fields V2-078's refusal needs to vary made explicit: provider.name and
// environment.granted_names. It exists because contracts rule 4 forbids a
// non-empty granted_names whenever provider.name is claude, so a fixture that
// declares one must name a different authorised provider -- otherwise the
// refusal would fire inside LoadPreflightRecord's schema validation instead of
// in the code under test. Every pre-existing caller reaches this through
// writeFixturePreflightRecord above with byte-identical behaviour.
func writeFixturePreflightRecordWithEnvironment(t *testing.T, repoRoot, dir, taskID, executablePath string, limits fixtureLimits, providerName string, baseNames, grantedNames []string) string {
	t.Helper()
	subjectPath := filepath.Join(repoRoot, ".agents", "v2", "packets", "V2-017-work-order.json")
	subjectBytes, err := os.ReadFile(subjectPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256Hex(subjectBytes)
	record := map[string]any{
		"schema_version": "v1",
		"id":             taskID + "-fixture",
		"kind":           "provider-preflight",
		"created_at":     "2026-08-25T04:00:00Z",
		"correlation_id": "costledger-fixture",
		"task_id":        taskID,
		"provider":       map[string]any{"name": providerName, "version": "2.1.241", "executable_path": executablePath},
		"limits": map[string]any{
			"max_invocations":            limits.MaxInvocations,
			"max_total_cost_usd":         limits.MaxTotalCostUSD,
			"worst_case_reservation_usd": limits.WorstCaseReservationUSD,
			"currency":                   "USD",
			"window":                     "total",
			"ledger_path":                filepath.Join(dir, "ledger.json"),
			"enforced_by":                "internal/runner.SupervisedInvocationRunner and internal/runner.CostLedger",
			"fail_closed":                true,
		},
		"environment": map[string]any{"base_names": baseNames, "granted_names": grantedNames},
		"rollback": map[string]any{
			"trigger":               "fixture",
			"argv":                  []string{"true"},
			"completion_conditions": []string{"fixture"},
		},
		"verification": []any{
			map[string]any{"name": "fixture", "argv": []string{"true"}, "expected": "fixture", "read_only": true},
		},
		"approval": map[string]any{
			"approver":           "takushi.yokoyama@sansan.com",
			"approved_at":        "2026-08-24T23:22:47Z",
			"approval_reference": "costledger-fixture-reference",
			"scope":              "costledger fixture",
			"subject_path":       ".agents/v2/packets/V2-017-work-order.json",
			"subject_digest":     digest,
		},
	}
	b, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, taskID+"-fixture.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runnerFixture bundles everything one fail-closed negative test needs.
type runnerFixture struct {
	runner     SupervisedInvocationRunner
	markerPath string
	ledgerPath string
	recordPath string
}

// newRunnerFixture builds a SupervisedInvocationRunner whose approved record
// points at a real, executable fake "claude" (basename matches, so only the
// specific condition under test can prevent execution), with limits
// generous enough that only the condition under test refuses Reserve.
func newRunnerFixture(t *testing.T) *runnerFixture {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	execPath := filepath.Join(dir, "claude")
	writeFakeExecutable(t, execPath, marker)
	recordPath := writeFixturePreflightRecord(t, mustRepoRoot(t), dir, "V2-999", execPath, fixtureLimits{MaxInvocations: 16, MaxTotalCostUSD: 10, WorstCaseReservationUSD: 1})
	log, err := NewBoundedLog(dir, "fixture-execution", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &runnerFixture{
		runner: SupervisedInvocationRunner{
			Supervisor: ProcessSupervisor{TermGrace: 2 * time.Second},
			Log:        log,
			Ledger:     &CostLedger{Path: filepath.Join(dir, "ledger.json"), Provider: "claude", TaskID: "V2-999"},
			RepoRoot:   mustRepoRoot(t),
			RecordPath: recordPath,
			Purpose:    "fixture-purpose",
		},
		markerPath: marker,
		ledgerPath: filepath.Join(dir, "ledger.json"),
		recordPath: recordPath,
	}
}

func (fx *runnerFixture) assertNoProcessStarted(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(fx.markerPath); err == nil {
		t.Fatal("a process WAS started: marker file is present")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// fixtureWorkingDirectory returns a directory that satisfies all five of
// V2-077's fail-closed properties (non-empty, absolute, canonical, an
// existing directory, not a symlink), so that the refusal each test below
// asserts is still the refusal that test was written to assert. Symlinks are
// resolved rather than assumed absent, because the process temporary
// directory is a symlink on some platforms.
func fixtureWorkingDirectory() string {
	if resolved, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(os.TempDir())
}

// invocationForFixture builds the Invocation every fail-closed negative test
// below drives Run with. WorkingDirectory is declared (V2-077) because
// SupervisedInvocationRunner.Run now refuses an unusable one inside step (1),
// strictly before LoadPreflightRecord and before Ledger.Reserve; without it
// every test below would refuse for that reason instead of the reason it
// exists to pin. No test name and no assertion below changed.
func invocationForFixture() provider.Invocation {
	return provider.Invocation{
		Argv:             []string{"claude", "--print", "--output-format", "json", "--no-session-persistence"},
		WorkingDirectory: fixtureWorkingDirectory(),
	}
}

// --- dp-v2-017 d3's nine fail-closed refusal cases. Each has its own test;
// each asserts, via marker-file absence, that no process was started. ---

func TestSupervisedInvocationRunnerFailsClosedLedgerUnreadable(t *testing.T) {
	fx := newRunnerFixture(t)
	if err := os.WriteFile(fx.ledgerPath, []byte(`{}`), 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(fx.ledgerPath, 0600) })
	_, err := fx.runner.Run(context.Background(), invocationForFixture())
	if !errors.Is(err, ErrCostLedgerUnreadable) {
		t.Fatalf("want ErrCostLedgerUnreadable, got %v", err)
	}
	fx.assertNoProcessStarted(t)
}

func TestSupervisedInvocationRunnerFailsClosedLedgerMalformed(t *testing.T) {
	fx := newRunnerFixture(t)
	if err := os.WriteFile(fx.ledgerPath, []byte(`not json at all`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := fx.runner.Run(context.Background(), invocationForFixture())
	if !errors.Is(err, ErrCostLedgerMalformed) {
		t.Fatalf("want ErrCostLedgerMalformed, got %v", err)
	}
	fx.assertNoProcessStarted(t)
}

func TestSupervisedInvocationRunnerFailsClosedRunMismatch(t *testing.T) {
	fx := newRunnerFixture(t)
	lf := ledgerFile{SchemaVersion: "v1", Provider: "codex", TaskID: "V2-999", Limits: ledgerFileLimits{MaxInvocations: 16, MaxTotalCostUSD: 10, WorstCaseReservationUSD: 1}}
	if err := writeLedgerAtomic(fx.ledgerPath, lf); err != nil {
		t.Fatal(err)
	}
	_, err := fx.runner.Run(context.Background(), invocationForFixture())
	if !errors.Is(err, ErrCostLedgerRunMismatch) {
		t.Fatalf("want ErrCostLedgerRunMismatch, got %v", err)
	}
	fx.assertNoProcessStarted(t)
}

func TestSupervisedInvocationRunnerFailsClosedHalted(t *testing.T) {
	fx := newRunnerFixture(t)
	record, err := LoadPreflightRecord(fx.runner.RepoRoot, fx.recordPath)
	if err != nil {
		t.Fatal(err)
	}
	lf := ledgerFile{
		SchemaVersion: "v1", Provider: "claude", TaskID: "V2-999", PreflightDigest: record.Digest,
		Limits: ledgerFileLimits{MaxInvocations: record.Limits.MaxInvocations, MaxTotalCostUSD: record.Limits.MaxTotalCostUSD, WorstCaseReservationUSD: record.Limits.WorstCaseReservationUSD},
		Halted: true,
	}
	if err := writeLedgerAtomic(fx.ledgerPath, lf); err != nil {
		t.Fatal(err)
	}
	_, err = fx.runner.Run(context.Background(), invocationForFixture())
	if !errors.Is(err, ErrCostLedgerHalted) {
		t.Fatalf("want ErrCostLedgerHalted, got %v", err)
	}
	fx.assertNoProcessStarted(t)
}

func TestSupervisedInvocationRunnerFailsClosedPreflightMissing(t *testing.T) {
	fx := newRunnerFixture(t)
	fx.runner.RecordPath = filepath.Join(t.TempDir(), "does-not-exist.json")
	_, err := fx.runner.Run(context.Background(), invocationForFixture())
	if !errors.Is(err, ErrCostLedgerPreflightInvalid) {
		t.Fatalf("want ErrCostLedgerPreflightInvalid, got %v", err)
	}
	fx.assertNoProcessStarted(t)
}

func TestSupervisedInvocationRunnerFailsClosedLimitsMismatch(t *testing.T) {
	fx := newRunnerFixture(t)
	record, err := LoadPreflightRecord(fx.runner.RepoRoot, fx.recordPath)
	if err != nil {
		t.Fatal(err)
	}
	lf := ledgerFile{
		SchemaVersion: "v1", Provider: "claude", TaskID: "V2-999", PreflightDigest: record.Digest,
		// A max_invocations that disagrees with the approved record's own
		// limits (16): this is case 6, distinct from case 9's budget-full
		// scenario, because the ledger's OWN recorded ceiling itself no
		// longer agrees with what the record currently declares.
		Limits: ledgerFileLimits{MaxInvocations: 1, MaxTotalCostUSD: record.Limits.MaxTotalCostUSD, WorstCaseReservationUSD: record.Limits.WorstCaseReservationUSD},
	}
	if err := writeLedgerAtomic(fx.ledgerPath, lf); err != nil {
		t.Fatal(err)
	}
	_, err = fx.runner.Run(context.Background(), invocationForFixture())
	if !errors.Is(err, ErrCostLedgerLimitsMismatch) {
		t.Fatalf("want ErrCostLedgerLimitsMismatch, got %v", err)
	}
	fx.assertNoProcessStarted(t)
}

func TestSupervisedInvocationRunnerFailsClosedExecutableMissing(t *testing.T) {
	fx := newRunnerFixture(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "nonexistent", "claude")
	recordPath := writeFixturePreflightRecord(t, mustRepoRoot(t), dir, "V2-998", missing, fixtureLimits{MaxInvocations: 16, MaxTotalCostUSD: 10, WorstCaseReservationUSD: 1})
	fx.runner.RecordPath = recordPath
	fx.runner.Ledger = &CostLedger{Path: filepath.Join(dir, "ledger.json"), Provider: "claude", TaskID: "V2-998"}
	_, err := fx.runner.Run(context.Background(), invocationForFixture())
	if !errors.Is(err, ErrCostLedgerExecutableMissing) {
		t.Fatalf("want ErrCostLedgerExecutableMissing, got %v", err)
	}
	fx.assertNoProcessStarted(t)
}

func TestSupervisedInvocationRunnerFailsClosedExecutableMismatch(t *testing.T) {
	fx := newRunnerFixture(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "wrong-marker")
	wrongPath := filepath.Join(dir, "not-claude")
	writeFakeExecutable(t, wrongPath, marker)
	recordPath := writeFixturePreflightRecord(t, mustRepoRoot(t), dir, "V2-997", wrongPath, fixtureLimits{MaxInvocations: 16, MaxTotalCostUSD: 10, WorstCaseReservationUSD: 1})
	fx.runner.RecordPath = recordPath
	fx.runner.Ledger = &CostLedger{Path: filepath.Join(dir, "ledger.json"), Provider: "claude", TaskID: "V2-997"}
	_, err := fx.runner.Run(context.Background(), invocationForFixture())
	if !errors.Is(err, ErrCostLedgerExecutableMismatch) {
		t.Fatalf("want ErrCostLedgerExecutableMismatch, got %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("a process WAS started: marker file is present")
	} else if !os.IsNotExist(statErr) {
		t.Fatal(statErr)
	}
}

func TestSupervisedInvocationRunnerFailsClosedOverBudget(t *testing.T) {
	fx := newRunnerFixture(t)
	record, err := LoadPreflightRecord(fx.runner.RepoRoot, fx.recordPath)
	if err != nil {
		t.Fatal(err)
	}
	// max_invocations=1 and one entry already present: the next Reserve's
	// count check (len(entries)+1 > max_invocations) refuses.
	dir := t.TempDir()
	recordPath := writeFixturePreflightRecord(t, mustRepoRoot(t), dir, "V2-996", record.ExecutablePath, fixtureLimits{MaxInvocations: 1, MaxTotalCostUSD: 10, WorstCaseReservationUSD: 1})
	fx.runner.RecordPath = recordPath
	rec2, err := LoadPreflightRecord(fx.runner.RepoRoot, recordPath)
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(dir, "ledger.json")
	lf := ledgerFile{
		SchemaVersion: "v1", Provider: "claude", TaskID: "V2-996", PreflightDigest: rec2.Digest,
		Limits:  ledgerFileLimits{MaxInvocations: 1, MaxTotalCostUSD: 10, WorstCaseReservationUSD: 1},
		Entries: []LedgerEntry{{Sequence: 1, Purpose: "prior", State: "settled", ReservedUSD: 1, ActualUSD: 0.1, StartedAt: time.Now().UTC()}},
	}
	if err := writeLedgerAtomic(ledgerPath, lf); err != nil {
		t.Fatal(err)
	}
	fx.runner.Ledger = &CostLedger{Path: ledgerPath, Provider: "claude", TaskID: "V2-996"}
	_, err = fx.runner.Run(context.Background(), invocationForFixture())
	if !errors.Is(err, ErrCostLedgerOverBudget) {
		t.Fatalf("want ErrCostLedgerOverBudget, got %v", err)
	}
	fx.assertNoProcessStarted(t)
}

// --- B4: a zero-value SupervisedInvocationRunner starts nothing. ---

func TestSupervisedInvocationRunnerFailsClosedOnZeroValue(t *testing.T) {
	var r SupervisedInvocationRunner
	_, err := r.Run(context.Background(), invocationForFixture())
	if !errors.Is(err, ErrSupervisedInvocationRunnerIncomplete) {
		t.Fatalf("want ErrSupervisedInvocationRunnerIncomplete, got %v", err)
	}
}

// --- B2/B5: CostLedger.Reserve/TrueUp mechanics, exercised directly. ---

func TestCostLedgerReserveWritesEntryAndTrueUpLowersNeverRaises(t *testing.T) {
	fx := newRunnerFixture(t)
	record, err := LoadPreflightRecord(fx.runner.RepoRoot, fx.recordPath)
	if err != nil {
		t.Fatal(err)
	}
	ledger := fx.runner.Ledger
	now := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	seq, err := ledger.Reserve(record, "claude", "test-reserve", now)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Fatalf("want sequence 1, got %d", seq)
	}
	snapBefore, err := ledger.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapBefore.ReservedTotalUSD != record.Limits.WorstCaseReservationUSD {
		t.Fatalf("reserved total = %v, want %v", snapBefore.ReservedTotalUSD, record.Limits.WorstCaseReservationUSD)
	}

	// TrueUp below the reservation frees budget: settled contribution is
	// the actual, lower figure.
	if err := ledger.TrueUp(seq, Settlement{ActualUSD: 0.05, SessionID: "sess-1"}, now); err != nil {
		t.Fatal(err)
	}
	snapAfter, err := ledger.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapAfter.ReservedTotalUSD != 0 {
		t.Fatalf("reserved total after settle = %v, want 0", snapAfter.ReservedTotalUSD)
	}
	if snapAfter.SettledTotalUSD != 0.05 {
		t.Fatalf("settled total = %v, want 0.05", snapAfter.SettledTotalUSD)
	}
	if snapAfter.Entries[0].ActualUSD != 0.05 || snapAfter.Entries[0].SessionID != "sess-1" || snapAfter.Entries[0].State != "settled" {
		t.Fatalf("unexpected entry: %#v", snapAfter.Entries[0])
	}

	// A second Reserve accounts the settled entry's contribution clamped at
	// its own ReservedUSD in the *budget formula* even though ActualUSD
	// itself remains the true 0.05 in the stored entry -- proven above --
	// so a settled entry never raises what is charged against future
	// budget beyond its own reservation.
	seq2, err := ledger.Reserve(record, "claude", "test-reserve-2", now)
	if err != nil {
		t.Fatal(err)
	}
	if seq2 != 2 {
		t.Fatalf("want sequence 2, got %d", seq2)
	}
}

func TestCostLedgerTrueUpRejectsNegativeActual(t *testing.T) {
	fx := newRunnerFixture(t)
	record, err := LoadPreflightRecord(fx.runner.RepoRoot, fx.recordPath)
	if err != nil {
		t.Fatal(err)
	}
	ledger := fx.runner.Ledger
	seq, err := ledger.Reserve(record, "claude", "test", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	err = ledger.TrueUp(seq, Settlement{ActualUSD: -1}, time.Now().UTC())
	if !errors.Is(err, ErrCostLedgerTrueUpExceedsReservation) {
		t.Fatalf("want ErrCostLedgerTrueUpExceedsReservation, got %v", err)
	}
}

// TestCostLedgerAnomalyHaltsAndPersistsToDisk is the B5 anomaly proof: an
// actual_usd above CostAnomalyThresholdUSD (2.00) sets halted:true, and a
// *freshly constructed* CostLedger value that only knows Path/Provider/
// TaskID -- i.e. one that reopens the file from disk rather than reusing
// any in-memory state -- sees the halt and refuses every later Reserve.
func TestCostLedgerAnomalyHaltsAndPersistsToDisk(t *testing.T) {
	fx := newRunnerFixture(t)
	record, err := LoadPreflightRecord(fx.runner.RepoRoot, fx.recordPath)
	if err != nil {
		t.Fatal(err)
	}
	ledger := fx.runner.Ledger
	seq, err := ledger.Reserve(record, "claude", "test-anomaly", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	err = ledger.TrueUp(seq, Settlement{ActualUSD: 3.50, SessionID: "sess-anomaly"}, time.Now().UTC())
	if err == nil {
		t.Fatal("expected a non-nil error from an anomalous settlement")
	}

	reopened := &CostLedger{Path: fx.ledgerPath, Provider: "claude", TaskID: "V2-999"}
	snap, err := reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Halted {
		t.Fatal("ledger reopened from disk is not halted")
	}
	if snap.Entries[0].ActualUSD != 3.50 {
		t.Fatalf("the true, unclamped anomalous actual_usd was not preserved: %#v", snap.Entries[0])
	}
	if _, err := reopened.Reserve(record, "claude", "test-after-halt", time.Now().UTC()); !errors.Is(err, ErrCostLedgerHalted) {
		t.Fatalf("want ErrCostLedgerHalted on a reopened halted ledger, got %v", err)
	}
}

// --- dp-v2-017 B19: the runaway-threshold handling is exercised, not just
// implemented. ---

func TestCostLedgerRunawayThresholdRefusesNextReserveAndStartsNoProcess(t *testing.T) {
	fx := newRunnerFixture(t)
	record, err := LoadPreflightRecord(fx.runner.RepoRoot, fx.recordPath)
	if err != nil {
		t.Fatal(err)
	}
	// Pre-load a scratch ledger already at max_invocations-1 settled
	// entries plus limits that leave no dollar headroom either, so the
	// *next* Reserve refuses under both the count and the dollar arm of
	// the dp-v2-017 d2 formula, proven by running the full
	// SupervisedInvocationRunner (so "no process starts" is checked, not
	// merely "Reserve returns an error").
	dir := t.TempDir()
	recordPath := writeFixturePreflightRecord(t, mustRepoRoot(t), dir, "V2-995", record.ExecutablePath, fixtureLimits{MaxInvocations: 1, MaxTotalCostUSD: 1, WorstCaseReservationUSD: 1})
	rec, err := LoadPreflightRecord(mustRepoRoot(t), recordPath)
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(dir, "ledger.json")
	lf := ledgerFile{
		SchemaVersion: "v1", Provider: "claude", TaskID: "V2-995", PreflightDigest: rec.Digest,
		Limits:  ledgerFileLimits{MaxInvocations: 1, MaxTotalCostUSD: 1, WorstCaseReservationUSD: 1},
		Entries: []LedgerEntry{{Sequence: 1, Purpose: "near-threshold", State: "settled", ReservedUSD: 1, ActualUSD: 0.95, StartedAt: time.Now().UTC()}},
	}
	if err := writeLedgerAtomic(ledgerPath, lf); err != nil {
		t.Fatal(err)
	}
	fx.runner.RecordPath = recordPath
	fx.runner.Ledger = &CostLedger{Path: ledgerPath, Provider: "claude", TaskID: "V2-995"}
	_, err = fx.runner.Run(context.Background(), invocationForFixture())
	if !errors.Is(err, ErrCostLedgerOverBudget) {
		t.Fatalf("want a runaway-suspected stop (ErrCostLedgerOverBudget), got %v", err)
	}
	fx.assertNoProcessStarted(t)

	snap, err := fx.runner.Ledger.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.InvocationCount != 1 {
		t.Fatalf("the refused attempt must not have appended a new entry: invocation_count=%d", snap.InvocationCount)
	}
}

// --- dp-v2-017 B7: the projection is doing real work, proven by a positive
// control. realCLIResponseShape is a synthetic literal that matches the
// real claude CLI's reported field set (total_cost_usd, usage, session_id,
// duration_api_ms, duration_ms, num_turns, is_error, type, subtype, result,
// uuid) with placeholder content -- never the actual captured text of any
// real invocation's response, per the constraint that no response text may
// be committed anywhere, evidence or otherwise. ---

const realCLIResponseShape = `{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"duration_api_ms":1100,"num_turns":1,"result":"placeholder result text, never real model output","session_id":"11111111-2222-3333-4444-555555555555","total_cost_usd":0.0842,"usage":{"input_tokens":120,"output_tokens":45,"cache_creation_input_tokens":800,"cache_read_input_tokens":0},"uuid":"66666666-7777-8888-9999-000000000000","permission_denials":[]}`

func TestClaudeAdapterParseRejectsRealShapeButProjectionSucceeds(t *testing.T) {
	// (a) Positive control: the real CLI's own JSON shape, handed straight
	// to the adapter, is refused. provider.parseFixture's
	// decoder.DisallowUnknownFields() rejects total_cost_usd, session_id,
	// duration_api_ms, duration_ms, is_error, num_turns, uuid and
	// permission_denials, none of which exist on the fixture struct.
	if _, err := (provider.ClaudeAdapter{}).Parse([]byte(realCLIResponseShape)); !errors.Is(err, provider.ErrInvalidFixture) {
		t.Fatalf("positive control failed: real CLI shape did not produce ErrInvalidFixture (got %v); the projection would not be doing real work", err)
	}

	// (b) The projection built from the same shape Parses successfully
	// with a non-empty OutputDigest, and never carries the response text
	// itself.
	projected, outcome, err := projectRealCLIResult([]byte(realCLIResponseShape), nil)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if outcome.Classification != "success" || outcome.SessionID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("unexpected outcome: %#v", outcome)
	}
	result, err := (provider.ClaudeAdapter{}).Parse(projected)
	if err != nil {
		t.Fatalf("projection failed to parse: %v", err)
	}
	if !result.Succeeded {
		t.Fatal("projected result did not succeed")
	}
	if result.OutputDigest == "" {
		t.Fatal("projected result has an empty OutputDigest")
	}
	if bytesContainsString(projected, "placeholder result text") {
		t.Fatal("the projection must never carry the response text itself")
	}
	if result.Checkpoint != "claude:11111111-2222-3333-4444-555555555555" {
		t.Fatalf("unexpected checkpoint: %s", result.Checkpoint)
	}
}

func bytesContainsString(b []byte, s string) bool {
	return strings.Contains(string(b), s)
}

// --- V2-078 A5: a declared grant the runner cannot deliver is refused, after
// the record is loaded and strictly before any reservation is debited. ---

// newGrantDeclaringRunnerFixture is newRunnerFixture with provider.name codex
// and an explicit environment.granted_names. codex rather than claude is
// forced by the contracts layer, not chosen for variety: rule 4 forbids a
// non-empty granted_names whenever provider.name is claude, so a claude
// fixture would be refused inside LoadPreflightRecord's schema validation and
// the refusal under test would never be reached. The fake executable, the
// ledger provider and argv[0] are all "codex" so that the empty-granted_names
// direction below is refused by nothing at all.
func newGrantDeclaringRunnerFixture(t *testing.T, grantedNames []string) *runnerFixture {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	execPath := filepath.Join(dir, "codex")
	writeFakeExecutable(t, execPath, marker)
	recordPath := writeFixturePreflightRecordWithEnvironment(t, mustRepoRoot(t), dir, "V2-994", execPath,
		fixtureLimits{MaxInvocations: 16, MaxTotalCostUSD: 10, WorstCaseReservationUSD: 1},
		"codex", []string{"HOME", "PATH"}, grantedNames)
	log, err := NewBoundedLog(dir, "grant-declaring-fixture-execution", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &runnerFixture{
		runner: SupervisedInvocationRunner{
			Supervisor: ProcessSupervisor{TermGrace: 2 * time.Second},
			Log:        log,
			Ledger:     &CostLedger{Path: filepath.Join(dir, "ledger.json"), Provider: "codex", TaskID: "V2-994"},
			RepoRoot:   mustRepoRoot(t),
			RecordPath: recordPath,
			Purpose:    "grant-declaring-fixture-purpose",
		},
		markerPath: marker,
		ledgerPath: filepath.Join(dir, "ledger.json"),
		recordPath: recordPath,
	}
}

// codexInvocationForFixture is invocationForFixture with argv[0] matching the
// codex fixture's executable basename, so Reserve's basename cross-check is
// not what refuses the empty-granted_names direction.
func codexInvocationForFixture() provider.Invocation {
	return provider.Invocation{
		Argv:             []string{"codex", "exec", "--json"},
		WorkingDirectory: fixtureWorkingDirectory(),
	}
}

// TestSupervisedInvocationRunnerRefusesAnUndeliverableEnvironmentGrant is
// V2-078's replacement for the field it deleted. Deleting
// provider.Invocation.Environment on its own would have converted a
// silently-dropped grant into a silently-ignored declaration: the approved
// record's environment.granted_names is schema-required, is semantically
// enforced (a non-empty value is forbidden for claude) and is bound by the
// approval digest, so it carries real enforcement that must be honoured
// rather than deleted. It is honoured by refusing.
//
// The ORDERING is asserted rather than commented, in both of the ways the
// package already asserts it: the ledger file must not exist afterwards
// (Reserve persists a reservation to disk before anything may execute, so an
// absent ledger file is proof Reserve was never reached) and the marker file
// must not exist (proof no process started). The fixture's limits would admit
// a reservation, so an absent ledger file is a fact about the ordering rather
// than a fact about the limits.
func TestSupervisedInvocationRunnerRefusesAnUndeliverableEnvironmentGrant(t *testing.T) {
	t.Run("declared_grant_is_refused_before_any_reservation", func(t *testing.T) {
		fx := newGrantDeclaringRunnerFixture(t, []string{"CODEX_TOKEN"})
		_, err := fx.runner.Run(context.Background(), codexInvocationForFixture())
		if !errors.Is(err, ErrInvocationEnvironmentGrantUndeliverable) {
			t.Fatalf("want ErrInvocationEnvironmentGrantUndeliverable, got %v", err)
		}
		fx.assertNoProcessStarted(t)
		if _, statErr := os.Stat(fx.ledgerPath); !os.IsNotExist(statErr) {
			t.Fatalf("a ledger file exists after the refusal, so the refusal did not precede Ledger.Reserve (stat error = %v)", statErr)
		}
		// The refusal names neither a credential nor a value: it reports
		// only that names were declared and that there is no channel.
		if strings.Contains(err.Error(), "CODEX_TOKEN") {
			t.Fatalf("the refusal must not echo a declared name back: %v", err)
		}
		t.Logf("execution fact (V2-078 A5): a record declaring 1 granted name is refused with no ledger file at %s and no process started; the reservation was never debited", fx.ledgerPath)
	})

	// Both directions, so the refusal is a fact about granted_names and not
	// a fact about codex, about this fixture, or about the fixture's limits.
	t.Run("empty_granted_names_is_not_refused_for_this_reason", func(t *testing.T) {
		fx := newGrantDeclaringRunnerFixture(t, []string{})
		_, err := fx.runner.Run(context.Background(), codexInvocationForFixture())
		if errors.Is(err, ErrInvocationEnvironmentGrantUndeliverable) {
			t.Fatalf("an empty environment.granted_names must not trigger the undeliverable-grant refusal, got %v", err)
		}
		if _, statErr := os.Stat(fx.ledgerPath); statErr != nil {
			t.Fatalf("with granted_names empty, Run must reach Ledger.Reserve and persist a reservation; stat %s: %v", fx.ledgerPath, statErr)
		}
		t.Logf("execution fact (V2-078 A5, other direction): the same fixture with environment.granted_names empty reaches Ledger.Reserve (ledger file present) and is refused by nothing on this account; Run returned err=%v", err)
	})
}
