package update

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRetentionReproducesTheReleasePredicateByValue asserts case-by-case
// agreement with internal/release's RetentionEligible (pipeline.go:83) on
// identical inputs, without importing it: dp-v2-021 d12 forbids that edge in
// both directions, so the agreement is a value-level equivalence -- the same
// four outcomes, with the same reason strings, in the same order.
//
// Every field this layer adds is left zero in these rows, so the extra
// refusals are inert and the parity is exact rather than approximate.
func TestRetentionReproducesTheReleasePredicateByValue(t *testing.T) {
	window := time.Hour
	cases := []struct {
		name     string
		in       RetentionInput
		eligible bool
		reason   string
	}{
		{
			name:   "current stable route target",
			in:     RetentionInput{Version: "1.0.0", CurrentStable: "1.0.0", PreviousStable: "0.9.0", RollbackWindow: window, Now: at(600)},
			reason: ReasonCurrentStable,
		},
		{
			name:   "previous stable route target",
			in:     RetentionInput{Version: "0.9.0", CurrentStable: "1.0.0", PreviousStable: "0.9.0", RollbackWindow: window, Now: at(600)},
			reason: ReasonPreviousStable,
		},
		{
			name:   "rollback window still open",
			in:     RetentionInput{Version: "0.8.0", CurrentStable: "1.0.0", PreviousStable: "0.9.0", RollbackWindow: window, RolledBackAt: at(0), Now: at(30)},
			reason: ReasonRollbackWindowOpen,
		},
		{
			name:   "a Requirement's StableSnapshot still references it",
			in:     RetentionInput{Version: "0.8.0", CurrentStable: "1.0.0", PreviousStable: "0.9.0", RollbackWindow: window, Now: at(600), ReferencedByRequirement: true},
			reason: ReasonRequirementReference,
		},
		{
			name:     "eligible",
			in:       RetentionInput{Version: "0.8.0", CurrentStable: "1.0.0", PreviousStable: "0.9.0", RollbackWindow: window, RolledBackAt: at(0), Now: at(600)},
			eligible: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eligible, reason := RetentionEligible(tc.in)
			if eligible != tc.eligible || reason != tc.reason {
				t.Fatalf("(%t, %q), want (%t, %q)", eligible, reason, tc.eligible, tc.reason)
			}
			t.Logf("verdict: eligible=%t reason=%q", eligible, reason)
		})
	}
	// The four reason strings are the contract with the release layer. A
	// reworded string here is a silent divergence, so they are pinned.
	pinned := []string{
		"version is the current stable route target",
		"version is the previous stable route target",
		"rollback window is still open",
		"a Requirement's StableSnapshot still references this version",
	}
	for i, want := range []string{ReasonCurrentStable, ReasonPreviousStable, ReasonRollbackWindowOpen, ReasonRequirementReference} {
		if want != pinned[i] {
			t.Fatalf("reason %d = %q, want %q", i, want, pinned[i])
		}
	}
}

// TestRetentionAddsTheRefusalsThatOnlyExistWithRealBinaries covers section
// 8's items 5 and 6 -- a live preview pointer, and the last version whose
// interval contains the canonical schema -- plus section 9.2's refusal to
// delete a version whose window closure was never recorded.
func TestRetentionAddsTheRefusalsThatOnlyExistWithRealBinaries(t *testing.T) {
	base := RetentionInput{Version: "0.8.0", CurrentStable: "1.0.0", PreviousStable: "0.9.0", RollbackWindow: time.Hour, Now: at(600)}
	cases := []struct {
		name     string
		mutate   func(*RetentionInput)
		eligible bool
		reason   string
	}{
		{
			name:   "the target of a preview symlink, which the release predicate does not model",
			mutate: func(in *RetentionInput) { in.ChannelTargets = []string{"1.0.0", "0.8.0"} },
			reason: ReasonChannelTarget,
		},
		{
			name:     "a channel set that does not name it",
			mutate:   func(in *RetentionInput) { in.ChannelTargets = []string{"1.0.0", "0.9.0"} },
			eligible: true,
		},
		{
			name: "the last retained version whose interval contains the canonical schema",
			mutate: func(in *RetentionInput) {
				in.CanonicalSchema = 2
				in.RetainedIntervals = map[string]SchemaInterval{"0.8.0": {Min: 1, Max: 2}, "1.0.0": {Min: 3, Max: 4}}
			},
			reason: ReasonLastSchemaCoverage,
		},
		{
			name: "one of several versions covering the canonical schema",
			mutate: func(in *RetentionInput) {
				in.CanonicalSchema = 2
				in.RetainedIntervals = map[string]SchemaInterval{"0.8.0": {Min: 1, Max: 2}, "1.0.0": {Min: 2, Max: 4}}
			},
			eligible: true,
		},
		{
			name:   "a window that opened and never recorded a closure",
			mutate: func(in *RetentionInput) { in.WindowOpened = true },
			reason: ReasonClosureNotRecorded,
		},
		{
			name: "a window with a recorded closure",
			mutate: func(in *RetentionInput) {
				in.WindowOpened = true
				in.WindowClosedAt = at(300)
			},
			eligible: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mutate(&in)
			eligible, reason := RetentionEligible(in)
			if eligible != tc.eligible || reason != tc.reason {
				t.Fatalf("(%t, %q), want (%t, %q)", eligible, reason, tc.eligible, tc.reason)
			}
			t.Logf("verdict: eligible=%t reason=%q", eligible, reason)
		})
	}
}

// TestCollectRefusesRoutableVersionsAndDeletesCrashSafely drives the executor
// on a real filesystem: the predicate first, then a re-read of every channel
// symlink immediately before deleting, then rename-aside, remove, and only
// then the record. It also shows the sweep that finishes a deletion a crash
// interrupted.
func TestCollectRefusesRoutableVersionsAndDeletesCrashSafely(t *testing.T) {
	f := newFixture(t)
	state := NewState(1)
	f.install(t, state, "1.0.0", []byte("one"), at(0))
	f.promote(t, state, "1.0.0", at(1))
	f.install(t, state, "2.0.0", []byte("two"), at(2))
	f.promote(t, state, "2.0.0", at(3))

	// A schema advance to 2, then a version that can operate against 2 and 3.
	state.CanonicalSchema = 2
	third, err := InstallRecorded(f.root, state, f.bundle(t, "3.0.0", []byte("three"), func(m *Manifest) {
		m.SchemaMin, m.SchemaMax = 2, 3
	}), f.anchors, at(4))
	if err != nil {
		t.Fatal(err)
	}
	if third.SchemaMin != 2 {
		t.Fatalf("manifest = %+v", third)
	}
	f.promote(t, state, "3.0.0", at(5))

	policy := WindowPolicy{MinimumDwell: time.Hour, ExerciseComplete: map[string]bool{"2.0.0": true, "3.0.0": true}, ContractApplied: true}

	t.Run("a-previous-stable-target-is-retained", func(t *testing.T) {
		in := state.RetentionInputFor("2.0.0", time.Hour, nil, at(600))
		if err := Collect(f.root, state, in); !errors.Is(err, ErrRetained) || !strings.Contains(err.Error(), ReasonPreviousStable) {
			t.Fatalf("err = %v", err)
		}
		t.Logf("verdict: %v", err)
		if _, statErr := os.Stat(VersionDir(f.root, "2.0.0")); statErr != nil {
			t.Fatal("a retained version was deleted")
		}
	})

	t.Run("an-open-window-is-retained", func(t *testing.T) {
		in := state.RetentionInputFor("1.0.0", time.Hour, nil, at(600))
		if err := Collect(f.root, state, in); !errors.Is(err, ErrRetained) || !strings.Contains(err.Error(), ReasonClosureNotRecorded) {
			t.Fatalf("err = %v", err)
		}
		t.Logf("verdict: %v", err)
	})

	// The contract step moves the canonical schema to 3, which is outside
	// 1.0.0's [1, 2]; every clause of its window has now ceased, so the
	// closure can be recorded -- and only a recorded closure makes it
	// deletable.
	state.CanonicalSchema = 3
	windowInput, err := state.WindowInputFor("1.0.0", policy, at(600))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CloseWindow(state, "1.0.0", windowInput); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(f.root, state); err != nil {
		t.Fatal(err)
	}

	t.Run("a-channel-symlink-is-re-read-immediately-before-deleting", func(t *testing.T) {
		// The snapshot says 1.0.0 is unreachable; the disk says preview
		// points at it. The refusal that matters is the one evaluated
		// against the disk as it is at the moment of deletion.
		in := state.RetentionInputFor("1.0.0", time.Hour, nil, at(600))
		if eligible, reason := RetentionEligible(in); !eligible {
			t.Fatalf("snapshot refused first: %s", reason)
		}
		link := filepath.Join(f.root, "preview")
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("versions", "1.0.0"), link); err != nil {
			t.Fatal(err)
		}
		if err := Collect(f.root, state, in); !errors.Is(err, ErrRetained) || !strings.Contains(err.Error(), ReasonChannelTarget) {
			t.Fatalf("err = %v", err)
		}
		t.Logf("verdict: %v", err)
		if _, statErr := os.Stat(VersionDir(f.root, "1.0.0")); statErr != nil {
			t.Fatal("a version reachable from a channel was deleted")
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("versions", "3.0.0"), link); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("residue-from-an-interrupted-deletion-is-swept-and-the-version-goes", func(t *testing.T) {
		residue := filepath.Join(f.root, "versions", gcPrefix+"0.5.0")
		if err := os.MkdirAll(filepath.Join(residue, "inner"), 0o700); err != nil {
			t.Fatal(err)
		}
		in := state.RetentionInputFor("1.0.0", time.Hour, nil, at(600))
		if err := Collect(f.root, state, in); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(residue); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("residue survived: %v", err)
		}
		if _, err := os.Stat(VersionDir(f.root, "1.0.0")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("1.0.0 survived collection: %v", err)
		}
		entries, err := os.ReadDir(filepath.Join(f.root, "versions"))
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if len(names) != 2 {
			t.Fatalf("versions/ = %v, want the two retained versions and no residue", names)
		}
		reloaded, err := LoadState(f.root, 3)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := reloaded.Find("1.0.0"); ok {
			t.Fatal("the record still names the deleted version")
		}
		for _, version := range []string{"2.0.0", "3.0.0"} {
			if _, ok := reloaded.Find(version); !ok {
				t.Fatalf("the record lost %s", version)
			}
		}
		t.Logf("verdict: versions/ = %v after collection, record lists %d versions", names, len(reloaded.Versions))
	})
}
