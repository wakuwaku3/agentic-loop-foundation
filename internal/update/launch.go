// The launcher: `bootstrap run --channel stable|preview`
// (docs/operations/self-update.md section 4.2).
//
// Measured gap this closes: cmd/bootstrap had verbs install|switch|--version
// only, and nothing in the tree read root/stable, so the pointer the Stable
// launcher is supposed to resolve had no reader at all.
//
// What may be claimed, and what may not. This launcher re-execs a
// re-verified binary after the child exits. That is NOT M8 completion
// condition 2, which names breaking a real Preview Control Plane and Runner
// and recovering from the Stable launcher: that is a preview-local exercise
// and belongs to V2-035. A launcher restarting a child in a unit test is not
// a recovery of a Preview environment.
//
// The launcher touches no canonical state and has no canonical store client
// at all, which is why a broken Preview Runner cannot prevent a Stable
// launcher from starting a Stable child.
package update

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

// ChildSpec is what the launcher asks its process port to start. The
// launcher never builds a command line itself, so the port can be a fake in
// a test and the real setpgid-setting starter in production.
type ChildSpec struct {
	Version string
	Path    string
	Args    []string
	Attempt int
	// NewProcessGroup is always true: a child started in the parent's
	// process group would take the launcher down with it.
	NewProcessGroup bool
}

// ChildResult is what the port reports back after the child exits.
type ChildResult struct {
	ExitCode int
}

// Launcher resolves a channel, re-verifies the bytes on disk, probes
// confinement and starts the Runner as a child. Every collaborator that
// touches the operating system beyond reading these files is injected, so
// the whole sequence is exercisable without a real Runner binary.
type Launcher struct {
	Root            string
	CanonicalSchema int
	Anchors         AnchorResolver

	// Probe is runner.NamespaceConfinement.Probe in production. A nil
	// probe is a refusal, not "no confinement required".
	Probe func(context.Context) error
	// Start runs the child and waits for it.
	Start func(context.Context, ChildSpec) (ChildResult, error)
	// Args is the argv tail handed to the Runner.
	Args []string
}

// LaunchOutcome accumulates what one Run did. Verifications counts the
// re-verifications actually performed, which is the observable form of "the
// bytes are verified before every exec".
type LaunchOutcome struct {
	Channel       string
	Version       string
	Verifications int
	Launches      int
	ExitCodes     []int
}

// ErrLaunchRefused is the launcher's fail-closed class.
var ErrLaunchRefused = errors.New("update: launch refused")

// Launch performs exactly one verified launch.
func (l Launcher) Launch(ctx context.Context, channel string) (LaunchOutcome, error) {
	return l.Run(ctx, channel, 1)
}

// Run performs up to maxLaunches verified launches, re-verifying the bytes
// on disk before each one. It stops early, with an error, at the first
// refusal; a refusal never starts a child and never writes into versions/.
func (l Launcher) Run(ctx context.Context, channel string, maxLaunches int) (LaunchOutcome, error) {
	outcome := LaunchOutcome{Channel: channel}
	if maxLaunches <= 0 {
		return outcome, fmt.Errorf("%w: a launch count must be positive", ErrLaunchRefused)
	}
	if l.Probe == nil || l.Start == nil {
		return outcome, fmt.Errorf("%w: a launcher without a confinement probe and a process starter cannot start a child", ErrLaunchRefused)
	}
	if l.CanonicalSchema <= 0 {
		return outcome, fmt.Errorf("%w: the canonical schema version is required to check a version's interval", ErrLaunchRefused)
	}
	for attempt := 1; attempt <= maxLaunches; attempt++ {
		// 1. The trust anchor, resolved from its fixed path, before
		// versions/ is read and before any child is started. It is
		// resolved on every attempt: an anchor that was replaced or
		// widened between two launches must be seen by the second one.
		anchors, err := l.Anchors.Resolve()
		if err != nil {
			return outcome, err
		}
		// 2. The channel pointer.
		version, err := ResolveChannel(l.Root, channel)
		if err != nil {
			return outcome, fmt.Errorf("%w: %v", ErrLaunchRefused, err)
		}
		outcome.Version = version
		// 3. Re-verification of the bytes as they are on disk now. This is
		// the same Verify that install ran, over freshly read bytes, and it
		// happens before every exec.
		manifest, err := VerifyInstalled(l.Root, version, anchors, l.CanonicalSchema)
		if err != nil {
			return outcome, fmt.Errorf("%w: %s: %v", ErrLaunchRefused, version, err)
		}
		outcome.Verifications++
		if !manifest.Covers(l.CanonicalSchema) {
			return outcome, fmt.Errorf("%w: %s declares schema interval [%d, %d], which excludes the canonical schema %d", ErrLaunchRefused, version, manifest.SchemaMin, manifest.SchemaMax, l.CanonicalSchema)
		}
		// 4. Confinement, refused rather than degraded (V2-046's idiom).
		if err := l.Probe(ctx); err != nil {
			return outcome, fmt.Errorf("%w: confinement probe failed: %w", ErrLaunchRefused, err)
		}
		// 5. The child, in its own process group.
		result, err := l.Start(ctx, ChildSpec{
			Version:         version,
			Path:            filepath.Join(VersionDir(l.Root, version), "runner"),
			Args:            append([]string(nil), l.Args...),
			Attempt:         attempt,
			NewProcessGroup: true,
		})
		if err != nil {
			return outcome, fmt.Errorf("%w: starting %s: %v", ErrLaunchRefused, version, err)
		}
		outcome.Launches++
		outcome.ExitCodes = append(outcome.ExitCodes, result.ExitCode)
		if err := ctx.Err(); err != nil {
			return outcome, err
		}
	}
	return outcome, nil
}
