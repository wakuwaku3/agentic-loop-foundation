// root/state/installed.json: the machine-local, never-canonical record of
// what this machine has installed and routed (docs/operations/self-update.md
// section 5.2).
//
// It is written *after* the filesystem operation it describes, so a crash
// leaves it stale-but-safe rather than describing a state that does not
// exist. It plus an injected clock is the entire input set of the GC
// predicate (section 8) and the rollback window (section 9): neither reads
// the wall clock, and neither reads anything else.
package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StateSchema is the id of the machine-local record. It is machine-local
// state, not canonical state, and is never promoted to either.
const StateSchema = "agentic-loop/installed-state/v1"

// InstalledVersion records one installed version and the coordinates the
// GC predicate, the window predicate and the interval check need. The
// schema interval is copied out of the signed manifest so a later decision
// never has to trust an unverified re-read.
type InstalledVersion struct {
	Version         string    `json:"version"`
	InstalledAt     time.Time `json:"installed_at"`
	ManifestSchema  string    `json:"manifest_schema"`
	SchemaMin       int       `json:"schema_min"`
	SchemaMax       int       `json:"schema_max"`
	BinarySHA256    string    `json:"binary_sha256"`
	BundleDigest    string    `json:"bundle_digest,omitempty"`
	CandidateID     string    `json:"candidate_id,omitempty"`
	ContractRelease string    `json:"contract_release,omitempty"`
}

// Covers reports whether this installed version can operate against schema.
func (v InstalledVersion) Covers(schema int) bool {
	return schema >= v.SchemaMin && schema <= v.SchemaMax
}

// SwitchRecord is the audit record gap item 11 named as missing: which
// version this machine routed, when, in which direction and why.
type SwitchRecord struct {
	Sequence        int             `json:"sequence"`
	At              time.Time       `json:"at"`
	Channel         string          `json:"channel"`
	From            string          `json:"from"`
	To              string          `json:"to"`
	Direction       SwitchDirection `json:"direction"`
	Reason          string          `json:"reason"`
	CandidateDigest string          `json:"candidate_digest,omitempty"`
}

// WindowState is one version's rollback window. Closure is recorded here
// with the criterion that closed it and is never recomputed from the wall
// clock at read time (section 9.2).
type WindowState struct {
	Opened            bool      `json:"opened"`
	SuccessorVersion  string    `json:"successor_version,omitempty"`
	SuccessorStableAt time.Time `json:"successor_stable_at,omitempty"`
	ClosedAt          time.Time `json:"window_closed_at,omitempty"`
	Criterion         string    `json:"window_closed_criterion,omitempty"`
}

// State is the whole machine-local record.
type State struct {
	Schema          string                 `json:"schema"`
	CanonicalSchema int                    `json:"canonical_schema"`
	Versions        []InstalledVersion     `json:"versions"`
	Stable          string                 `json:"stable,omitempty"`
	Preview         string                 `json:"preview,omitempty"`
	PreviousStable  string                 `json:"previous_stable,omitempty"`
	Switches        []SwitchRecord         `json:"switches,omitempty"`
	Windows         map[string]WindowState `json:"windows,omitempty"`
	RolledBackAt    map[string]time.Time   `json:"rolled_back_at,omitempty"`
}

// NewState is an empty record for a machine with nothing installed.
func NewState(canonicalSchema int) *State {
	return &State{Schema: StateSchema, CanonicalSchema: canonicalSchema, Windows: map[string]WindowState{}, RolledBackAt: map[string]time.Time{}}
}

// StatePath is the fixed location of the machine-local record.
func StatePath(root string) string {
	return filepath.Join(root, "state", "installed.json")
}

// LoadState reads the record, returning an empty one when this machine has
// never installed anything. A record declaring an unknown schema is a
// refusal, not a value to guess at.
func LoadState(root string, canonicalSchema int) (*State, error) {
	if !validRoot(root) {
		return nil, errors.New("update root must be an explicit absolute directory")
	}
	data, err := os.ReadFile(StatePath(root))
	if errors.Is(err, os.ErrNotExist) {
		return NewState(canonicalSchema), nil
	}
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("installed state is not readable: %w", err)
	}
	if state.Schema != StateSchema {
		return nil, fmt.Errorf("installed state declares schema %q, want %q", state.Schema, StateSchema)
	}
	if state.Windows == nil {
		state.Windows = map[string]WindowState{}
	}
	if state.RolledBackAt == nil {
		state.RolledBackAt = map[string]time.Time{}
	}
	return &state, nil
}

// SaveState writes the record atomically. Callers call it only after the
// filesystem operation it describes has completed.
func SaveState(root string, state *State) error {
	if !validRoot(root) || state == nil {
		return errors.New("invalid installed state write")
	}
	state.Schema = StateSchema
	dir := filepath.Dir(StatePath(root))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// MarshalIndent with map keys sorted by encoding/json makes the bytes a
	// function of the value alone, so a reviewer diffing two records sees
	// only what changed.
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(dir, ".installed-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, StatePath(root))
}

// Find returns the record for version, if this machine installed it.
func (s *State) Find(version string) (InstalledVersion, bool) {
	for _, v := range s.Versions {
		if v.Version == version {
			return v, true
		}
	}
	return InstalledVersion{}, false
}

// RecordInstalled adds or refreshes one installed version. Install is
// idempotent, so recording it twice must not duplicate the entry.
func (s *State) RecordInstalled(manifest Manifest, at time.Time) {
	entry := InstalledVersion{
		Version:         manifest.Version,
		InstalledAt:     at,
		ManifestSchema:  manifest.Schema,
		SchemaMin:       manifest.SchemaMin,
		SchemaMax:       manifest.SchemaMax,
		BinarySHA256:    manifest.BinarySHA256,
		BundleDigest:    manifest.BundleDigest,
		CandidateID:     manifest.CandidateID,
		ContractRelease: manifest.ContractRelease,
	}
	for i, v := range s.Versions {
		if v.Version == entry.Version {
			entry.InstalledAt = v.InstalledAt
			s.Versions[i] = entry
			return
		}
	}
	s.Versions = append(s.Versions, entry)
}

// ChannelTargets is every channel this record routes, current values only.
func (s *State) ChannelTargets() []string {
	out := make([]string, 0, 2)
	for _, v := range []string{s.Stable, s.Preview} {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// window returns a version's window state, zero-valued when it has none.
func (s *State) window(version string) WindowState {
	if s.Windows == nil {
		return WindowState{}
	}
	return s.Windows[version]
}

func (s *State) setWindow(version string, w WindowState) {
	if s.Windows == nil {
		s.Windows = map[string]WindowState{}
	}
	s.Windows[version] = w
}
