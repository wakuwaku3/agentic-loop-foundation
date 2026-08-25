// The GC predicate and its executor (docs/operations/self-update.md
// section 8), plus the second use of the same predicate as the contract
// stage's precondition (section 7.2).
//
// dp-v2-021 d12 forbids an import edge between internal/release and
// internal/update in either direction, so release.RetentionEligible's four
// outcomes are reproduced here BY VALUE: the same four refusal strings in
// the same order over the same inputs, asserted case by case in
// retention_test.go, and guarded in the other direction by the mirror-image
// go/ast guard in source_guard_test.go. That is a value-level equivalence,
// not a code edge.
//
// Two further refusals exist only once binaries are real, and one more
// exists because section 9.2 forbids deleting a version whose window
// closure was never recorded. All three are inert on the inputs the
// release-parity table uses, so parity is exact.
package update

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The four refusal strings reproduced by value from
// internal/release/pipeline.go's RetentionEligible (pipeline.go:83), in its
// order. Changing any string here breaks the agreement test on purpose.
const (
	ReasonCurrentStable        = "version is the current stable route target"
	ReasonPreviousStable       = "version is the previous stable route target"
	ReasonRollbackWindowOpen   = "rollback window is still open"
	ReasonRequirementReference = "a Requirement's StableSnapshot still references this version"
)

// The refusals this layer adds. The first two are section 8's items 5 and
// 6; the third is section 9.2's "deleting a version with no recorded
// window_closed_at" violation stated as a refusal.
const (
	ReasonChannelTarget      = "version is the target of a channel symlink on this machine"
	ReasonLastSchemaCoverage = "deleting this version would leave no retained version whose schema interval contains the canonical schema"
	ReasonClosureNotRecorded = "the rollback window opened and its closure was never recorded"
)

// SchemaInterval is one retained version's closed canonical-schema interval.
type SchemaInterval struct {
	Min int
	Max int
}

// Covers reports whether schema is inside this closed interval.
func (i SchemaInterval) Covers(schema int) bool { return schema >= i.Min && schema <= i.Max }

// RetentionInput is the injected-clock, contract-window state the predicate
// needs. Nothing here reads the wall clock. The first seven fields are
// release.RetentionInput's fields under the same names and meanings; the
// rest are this layer's additions and are inert when left zero.
type RetentionInput struct {
	Version                 string
	CurrentStable           string
	PreviousStable          string
	RollbackWindow          time.Duration
	RolledBackAt            time.Time // zero if this version was never rolled back away from
	Now                     time.Time
	ReferencedByRequirement bool

	// ChannelTargets is every channel symlink target on this machine,
	// including preview. release.RetentionEligible models stable and
	// previous-stable only, but preview is a live pointer.
	ChannelTargets []string

	// WindowOpened records that this version's rollback window ever opened;
	// WindowClosedAt is its recorded closure. Together they express section
	// 9.2: a version whose window opened and never recorded a closure is
	// not deletable, and the answer never depends on the wall clock.
	WindowOpened   bool
	WindowClosedAt time.Time

	// RetainedIntervals is every version this machine would still hold,
	// this one included, keyed by version. When it is empty the coverage
	// refusal is not evaluated; the GC executor always supplies it.
	CanonicalSchema   int
	RetainedIntervals map[string]SchemaInterval
}

// RetentionEligible is a pure function: a version may be deleted only when
// every refusal below is false. The first four are release-parity, in
// release's order, so identical inputs give identical outcomes.
func RetentionEligible(in RetentionInput) (bool, string) {
	if in.Version == in.CurrentStable {
		return false, ReasonCurrentStable
	}
	if in.Version == in.PreviousStable {
		return false, ReasonPreviousStable
	}
	if !in.RolledBackAt.IsZero() && in.Now.Before(in.RolledBackAt.Add(in.RollbackWindow)) {
		return false, ReasonRollbackWindowOpen
	}
	if in.ReferencedByRequirement {
		return false, ReasonRequirementReference
	}
	for _, target := range in.ChannelTargets {
		if target == in.Version {
			return false, ReasonChannelTarget
		}
	}
	if in.WindowOpened && in.WindowClosedAt.IsZero() {
		return false, ReasonClosureNotRecorded
	}
	if len(in.RetainedIntervals) > 0 && in.CanonicalSchema > 0 {
		covered := false
		for version, interval := range in.RetainedIntervals {
			if version == in.Version {
				continue
			}
			if interval.Covers(in.CanonicalSchema) {
				covered = true
				break
			}
		}
		if !covered {
			return false, ReasonLastSchemaCoverage
		}
	}
	return true, ""
}

// RetentionInputFor assembles the predicate's input for one version out of
// the machine-local record, the contract-declared window and the injected
// clock. referenced names the versions some Requirement's StableSnapshot
// still points at.
func (s *State) RetentionInputFor(version string, window time.Duration, referenced map[string]bool, now time.Time) RetentionInput {
	intervals := map[string]SchemaInterval{}
	for _, v := range s.Versions {
		intervals[v.Version] = SchemaInterval{Min: v.SchemaMin, Max: v.SchemaMax}
	}
	w := s.window(version)
	return RetentionInput{
		Version:                 version,
		CurrentStable:           s.Stable,
		PreviousStable:          s.PreviousStable,
		RollbackWindow:          window,
		RolledBackAt:            s.RolledBackAt[version],
		Now:                     now,
		ReferencedByRequirement: referenced[version],
		ChannelTargets:          s.ChannelTargets(),
		WindowOpened:            w.Opened,
		WindowClosedAt:          w.ClosedAt,
		CanonicalSchema:         s.CanonicalSchema,
		RetainedIntervals:       intervals,
	}
}

// ErrRetained refuses a deletion the predicate did not permit.
var ErrRetained = errors.New("update: version is retained")

// gcPrefix marks a directory that has been renamed aside for deletion. A
// crash between the rename and the removal leaves one behind; SweepGCResidue
// finishes the job on the next pass, which is why the rename comes first.
const gcPrefix = ".gc-"

// SweepGCResidue removes directories left renamed-aside by a crash. It is
// the first step of every collection pass.
func SweepGCResidue(root string) (int, error) {
	if !validRoot(root) {
		return 0, errors.New("update root must be an explicit absolute directory")
	}
	versions := filepath.Join(root, "versions")
	entries, err := os.ReadDir(versions)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), gcPrefix) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if err := os.RemoveAll(filepath.Join(versions, name)); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// Collect deletes one version's directory, in the crash-safe order of
// section 8: sweep residue, evaluate the predicate, re-read every channel
// symlink from disk immediately before deleting and refuse if the version is
// reachable from any of them, rename the directory aside, remove it, and
// only then update the machine-local record.
//
// The re-read is not a duplicate of the ChannelTargets refusal: the
// predicate's input is a snapshot, and a switch may have happened since it
// was taken. The refusal that matters is the one evaluated against the disk
// as it is at the moment of deletion.
func Collect(root string, state *State, in RetentionInput) error {
	if !validRoot(root) || state == nil {
		return errors.New("invalid collection request")
	}
	if !versionPattern.MatchString(in.Version) {
		return errors.New("invalid version reference")
	}
	if _, err := SweepGCResidue(root); err != nil {
		return err
	}
	if eligible, reason := RetentionEligible(in); !eligible {
		return fmt.Errorf("%w: %s: %s", ErrRetained, in.Version, reason)
	}
	for _, channel := range Channels {
		routed, err := ResolveChannel(root, channel)
		if err != nil {
			continue // an unrouted channel cannot make a version reachable
		}
		if routed == in.Version {
			return fmt.Errorf("%w: %s: %s (%s)", ErrRetained, in.Version, ReasonChannelTarget, channel)
		}
	}
	dir := VersionDir(root, in.Version)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("installed version not found: %s", in.Version)
	} else if err != nil {
		return err
	}
	aside := filepath.Join(root, "versions", gcPrefix+in.Version)
	if err := os.RemoveAll(aside); err != nil {
		return err
	}
	if err := os.Rename(dir, aside); err != nil {
		return err
	}
	if err := os.RemoveAll(aside); err != nil {
		return err
	}
	// Only now does the record stop naming the version.
	kept := make([]InstalledVersion, 0, len(state.Versions))
	for _, v := range state.Versions {
		if v.Version != in.Version {
			kept = append(kept, v)
		}
	}
	state.Versions = kept
	delete(state.Windows, in.Version)
	delete(state.RolledBackAt, in.Version)
	return SaveState(root, state)
}

// VersionRetention pairs one version's schema floor with its retention
// input, which is all the contract-stage precondition needs.
type VersionRetention struct {
	Version   string
	SchemaMin int
	Retention RetentionInput
}

// ErrContractRefused refuses a contract step that would strand a version.
var ErrContractRefused = errors.New("update: contract stage refused")

// ContractAllowed is the second use of RetentionEligible, and deliberately
// the same predicate rather than a parallel one: a version that is not
// GC-eligible is still routable, and contract may run only when every such
// version's schema_min is at or above the post-contract schema. Expressing
// "may I contract?" and "may I delete?" once stops the two from disagreeing.
func ContractAllowed(postContractSchema int, versions []VersionRetention) (bool, string) {
	if postContractSchema <= 0 {
		return false, "post-contract schema must be positive"
	}
	blocking := make([]string, 0, len(versions))
	for _, v := range versions {
		if eligible, _ := RetentionEligible(v.Retention); eligible {
			continue // deletable, so it constrains nothing
		}
		if v.SchemaMin < postContractSchema {
			blocking = append(blocking, fmt.Sprintf("%s (schema_min %d)", v.Version, v.SchemaMin))
		}
	}
	if len(blocking) > 0 {
		sort.Strings(blocking)
		return false, fmt.Sprintf("still-retained versions cannot read schema %d: %s", postContractSchema, strings.Join(blocking, ", "))
	}
	return true, ""
}
