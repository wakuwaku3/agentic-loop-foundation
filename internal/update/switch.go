// The recorded, monotonic channel switch (docs/operations/self-update.md
// section 9.3).
//
// The measured defect: update.Switch flipped a channel to any installed
// version with no record, no monotonicity and no requirement that a forward
// move name a gate-passed candidate. dp-v2-021 d8 had already paid to
// remove exactly that ping-pong defect from release.Router, so leaving it
// here would re-introduce it one layer down. This file mirrors the fix by
// value: every switch is recorded with its reason, a second consecutive
// rollback is refused because the first clears the previous-stable pointer,
// and a forward move must name the candidate the signed manifest carries.
package update

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// SwitchDirection distinguishes the two moves. It is part of the request,
// not inferred from a version ordering, because "newer" is not a total
// order over pre-release versions and a caller must state what it means.
type SwitchDirection string

const (
	SwitchForward  SwitchDirection = "forward"
	SwitchRollback SwitchDirection = "rollback"
)

// SwitchRequest is one routing change. Reason is required: an unexplained
// switch is the audit gap of item 11.
type SwitchRequest struct {
	Channel         string
	Version         string
	Direction       SwitchDirection
	Reason          string
	CandidateDigest string
}

// ErrRollbackExhausted reports the refused second consecutive rollback: the
// first rollback cleared the previous-stable pointer, so there is nothing
// left to roll back to without a forward move in between.
var ErrRollbackExhausted = errors.New("update: rollback refused, this machine has no recorded previous stable target")

// Switch atomically changes a channel pointer without deleting its prior
// target, and records the change in the machine-local state afterwards.
// Rollback is the same operation with the recorded previous stable version.
//
// The state is saved after the symlink rename, so a crash between the two
// leaves a record that under-claims (it still names the old target) rather
// than one that names a route that does not exist.
func Switch(root string, state *State, req SwitchRequest, now time.Time) error {
	if !validRoot(root) || state == nil || !ChannelName(req.Channel) || !versionPattern.MatchString(req.Version) {
		return errors.New("invalid update switch target")
	}
	if req.Reason == "" {
		return errors.New("update switch refused: every switch must record a reason")
	}
	if req.Direction != SwitchForward && req.Direction != SwitchRollback {
		return fmt.Errorf("update switch refused: unknown direction %q", req.Direction)
	}
	info, err := os.Lstat(VersionDir(root, req.Version))
	if err != nil || !info.IsDir() {
		return fmt.Errorf("installed version not found: %s", req.Version)
	}
	installed, ok := state.Find(req.Version)
	if !ok {
		return fmt.Errorf("installed version %s is not recorded in the machine state", req.Version)
	}
	// The single-machine half of the section 6 invariant: a version may be
	// routed only if the canonical schema this machine reads is inside its
	// interval. The cross-machine half needs Runner version reporting and
	// is escalation E1 (V2-069); it is deliberately not attempted here.
	if !installed.Covers(state.CanonicalSchema) {
		return fmt.Errorf("update switch refused: version %s declares schema interval [%d, %d], which excludes the canonical schema %d", req.Version, installed.SchemaMin, installed.SchemaMax, state.CanonicalSchema)
	}

	from := state.Stable
	if req.Channel == "preview" {
		from = state.Preview
	}
	if from == req.Version {
		return fmt.Errorf("update switch refused: %s is already the %s route target", req.Version, req.Channel)
	}

	switch req.Direction {
	case SwitchForward:
		// A forward move must name the gate-passed candidate the signed
		// manifest carries. A version whose manifest has no candidate --
		// every manifest declaring the older accepted schema id -- can
		// therefore never be routed forward.
		if installed.CandidateID == "" {
			return fmt.Errorf("update switch refused: version %s carries no candidate id, so no forward move can name a gate-passed candidate", req.Version)
		}
		if req.CandidateDigest == "" || req.CandidateDigest != installed.CandidateID {
			return fmt.Errorf("update switch refused: forward move must name version %s's gate-passed candidate", req.Version)
		}
		// Mirror of release.Promote: stable advances only onto what preview
		// currently routes, so stable cannot jump to an arbitrary installed
		// version.
		if req.Channel == "stable" && state.Preview != req.Version {
			return fmt.Errorf("update switch refused: stable advances only onto the current preview route target (%q)", state.Preview)
		}
	case SwitchRollback:
		if req.Channel != "stable" {
			return errors.New("update switch refused: rollback is a stable-channel operation")
		}
		if state.PreviousStable == "" {
			return ErrRollbackExhausted
		}
		if state.PreviousStable != req.Version {
			return fmt.Errorf("update switch refused: the recorded previous stable target is %q", state.PreviousStable)
		}
	}

	if err := linkChannel(root, req.Channel, req.Version); err != nil {
		return err
	}

	// Everything below this line describes an operation that has already
	// happened on disk.
	if req.Channel == "preview" {
		state.Preview = req.Version
	} else {
		state.Stable = req.Version
		switch req.Direction {
		case SwitchForward:
			state.PreviousStable = from
			if from != "" {
				w := state.window(from)
				w.Opened = true
				w.SuccessorVersion = req.Version
				w.SuccessorStableAt = now
				state.setWindow(from, w)
			}
		case SwitchRollback:
			// The rollback consumes the previous-stable pointer, which is
			// what makes a second consecutive rollback a refusal rather
			// than a ping-pong.
			state.PreviousStable = ""
			if state.RolledBackAt == nil {
				state.RolledBackAt = map[string]time.Time{}
			}
			if from != "" {
				state.RolledBackAt[from] = now
			}
		}
	}
	state.Switches = append(state.Switches, SwitchRecord{
		Sequence:        len(state.Switches) + 1,
		At:              now,
		Channel:         req.Channel,
		From:            from,
		To:              req.Version,
		Direction:       req.Direction,
		Reason:          req.Reason,
		CandidateDigest: req.CandidateDigest,
	})
	return SaveState(root, state)
}

// InstallRecorded is Install plus the machine-local record of it, written
// after the version directory exists.
func InstallRecorded(root string, state *State, bundle Bundle, anchors AnchorSet, now time.Time) (Manifest, error) {
	if state == nil {
		return Manifest{}, errors.New("invalid installed state")
	}
	manifest, err := Install(root, bundle, anchors, state.CanonicalSchema)
	if err != nil {
		return Manifest{}, err
	}
	state.RecordInstalled(manifest, now)
	if err := SaveState(root, state); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}
