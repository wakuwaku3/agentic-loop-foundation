package update

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSwitchIsRecordedMonotonicAndGated is the mirror of dp-v2-021 d8: the
// defect update.Switch still had -- flip to any installed version, no
// record, no monotonicity, no gate -- is refused in each of its forms, and
// every accepted switch leaves an audit record.
func TestSwitchIsRecordedMonotonicAndGated(t *testing.T) {
	f := newFixture(t)
	state := NewState(1)
	f.install(t, state, "1.0.0", []byte("one"), at(0))
	f.install(t, state, "2.0.0", []byte("two"), at(1))
	older := f.bundle(t, "0.9.0", []byte("older"), func(m *Manifest) {
		m.Schema = ManifestSchemaV1
		m.BundleDigest, m.CandidateID, m.ContractRelease, m.ContractDigest = "", "", "", ""
		m.RunnerAPIMin, m.RunnerAPIMax = 0, 0
	})
	if _, err := InstallRecorded(f.root, state, older, f.anchors, at(2)); err != nil {
		t.Fatal(err)
	}

	t.Run("a-switch-without-a-reason-is-refused", func(t *testing.T) {
		err := Switch(f.root, state, SwitchRequest{Channel: "preview", Version: "1.0.0", Direction: SwitchForward, CandidateDigest: candidateOf("1.0.0")}, at(3))
		if err == nil {
			t.Fatal("accepted")
		}
		t.Logf("verdict: %v", err)
	})
	t.Run("a-forward-move-must-name-the-gate-passed-candidate", func(t *testing.T) {
		for _, digest := range []string{"", candidateOf("2.0.0")} {
			err := Switch(f.root, state, SwitchRequest{Channel: "preview", Version: "1.0.0", Direction: SwitchForward, Reason: "r", CandidateDigest: digest}, at(3))
			if err == nil {
				t.Fatalf("candidate %q accepted for 1.0.0", digest)
			}
			t.Logf("verdict candidate=%q: %v", digest, err)
		}
	})
	t.Run("a-version-with-no-candidate-can-never-move-forward", func(t *testing.T) {
		err := Switch(f.root, state, SwitchRequest{Channel: "preview", Version: "0.9.0", Direction: SwitchForward, Reason: "r", CandidateDigest: "anything"}, at(3))
		if err == nil {
			t.Fatal("a join-less version was routed forward")
		}
		t.Logf("verdict: %v", err)
	})
	t.Run("an-unknown-direction-is-refused", func(t *testing.T) {
		err := Switch(f.root, state, SwitchRequest{Channel: "preview", Version: "1.0.0", Direction: "sideways", Reason: "r", CandidateDigest: candidateOf("1.0.0")}, at(3))
		if err == nil {
			t.Fatal("accepted")
		}
		t.Logf("verdict: %v", err)
	})
	t.Run("rollback-is-a-stable-channel-operation", func(t *testing.T) {
		err := Switch(f.root, state, SwitchRequest{Channel: "preview", Version: "1.0.0", Direction: SwitchRollback, Reason: "r"}, at(3))
		if err == nil {
			t.Fatal("accepted")
		}
		t.Logf("verdict: %v", err)
	})

	// The accepted path: preview first, then stable, because stable advances
	// only onto the current preview route target.
	t.Run("stable-advances-only-onto-the-current-preview-route", func(t *testing.T) {
		if err := Switch(f.root, state, SwitchRequest{Channel: "preview", Version: "1.0.0", Direction: SwitchForward, Reason: "stage 1.0.0", CandidateDigest: candidateOf("1.0.0")}, at(4)); err != nil {
			t.Fatal(err)
		}
		err := Switch(f.root, state, SwitchRequest{Channel: "stable", Version: "2.0.0", Direction: SwitchForward, Reason: "jump", CandidateDigest: candidateOf("2.0.0")}, at(5))
		if err == nil {
			t.Fatal("stable jumped to a version preview does not route")
		}
		t.Logf("verdict: %v", err)
		if err := Switch(f.root, state, SwitchRequest{Channel: "stable", Version: "1.0.0", Direction: SwitchForward, Reason: "adopt 1.0.0", CandidateDigest: candidateOf("1.0.0")}, at(6)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("re-routing-to-the-current-target-is-refused", func(t *testing.T) {
		err := Switch(f.root, state, SwitchRequest{Channel: "stable", Version: "1.0.0", Direction: SwitchForward, Reason: "again", CandidateDigest: candidateOf("1.0.0")}, at(7))
		if err == nil {
			t.Fatal("accepted")
		}
		t.Logf("verdict: %v", err)
	})
	t.Run("the-first-rollback-succeeds-and-the-second-consecutive-one-is-refused", func(t *testing.T) {
		f.promote(t, state, "2.0.0", at(8))
		if state.PreviousStable != "1.0.0" {
			t.Fatalf("previous stable = %q", state.PreviousStable)
		}
		if err := Switch(f.root, state, SwitchRequest{Channel: "stable", Version: "1.0.0", Direction: SwitchRollback, Reason: "successor regressed"}, at(9)); err != nil {
			t.Fatal(err)
		}
		if state.PreviousStable != "" {
			t.Fatalf("the rollback did not consume the previous stable pointer: %q", state.PreviousStable)
		}
		err := Switch(f.root, state, SwitchRequest{Channel: "stable", Version: "2.0.0", Direction: SwitchRollback, Reason: "ping-pong"}, at(10))
		if !errors.Is(err, ErrRollbackExhausted) {
			t.Fatalf("second consecutive rollback err = %v, want ErrRollbackExhausted", err)
		}
		t.Logf("verdict: %v", err)
		// A forward move re-establishes a previous stable target, so the
		// refusal is of consecutive rollbacks, not of rollback itself.
		if err := Switch(f.root, state, SwitchRequest{Channel: "stable", Version: "2.0.0", Direction: SwitchForward, Reason: "re-adopt the successor", CandidateDigest: candidateOf("2.0.0")}, at(11)); err != nil {
			t.Fatal(err)
		}
		if err := Switch(f.root, state, SwitchRequest{Channel: "stable", Version: "1.0.0", Direction: SwitchRollback, Reason: "second regression"}, at(12)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a-version-whose-interval-excludes-the-canonical-schema-cannot-be-routed", func(t *testing.T) {
		advanced := *state
		advanced.CanonicalSchema = 5
		err := Switch(f.root, &advanced, SwitchRequest{Channel: "preview", Version: "1.0.0", Direction: SwitchForward, Reason: "r", CandidateDigest: candidateOf("1.0.0")}, at(13))
		if err == nil {
			t.Fatal("a version outside the canonical schema interval was routed")
		}
		t.Logf("verdict: %v", err)
	})
	t.Run("every-accepted-switch-is-recorded", func(t *testing.T) {
		reloaded, err := LoadState(f.root, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(reloaded.Switches) == 0 {
			t.Fatal("no switch was recorded")
		}
		for _, record := range reloaded.Switches {
			if record.Reason == "" || record.Channel == "" || record.To == "" || record.Direction == "" || record.Sequence == 0 {
				t.Fatalf("incomplete switch record: %+v", record)
			}
		}
		last := reloaded.Switches[len(reloaded.Switches)-1]
		if last.Direction != SwitchRollback || last.To != "1.0.0" || last.From != "2.0.0" {
			t.Fatalf("last record = %+v", last)
		}
		t.Logf("verdict: %d switch records, last = %s %s->%s (%s)", len(reloaded.Switches), last.Direction, last.From, last.To, last.Reason)
		// No switch, accepted or refused, deleted a version directory.
		for _, version := range []string{"0.9.0", "1.0.0", "2.0.0"} {
			if _, err := os.Stat(filepath.Join(VersionDir(f.root, version), "runner")); err != nil {
				t.Fatalf("version %s lost: %v", version, err)
			}
		}
	})
}
