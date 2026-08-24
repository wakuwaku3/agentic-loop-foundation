package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// TestControlIntentKillsRealProcessGroupThroughControlLoop is acceptance A9:
// a control intent causes a real process group to die, proven with a real
// signal, driven end to end through ControlLoop.Apply -- not by calling
// context.CancelFunc directly the way
// TestSupervisorCancelsProcessGroupTERMThenKILL does (that sibling test,
// which this one deliberately does not modify or duplicate, already proves
// ProcessSupervisor's own TERM-then-KILL group termination on a bare
// context cancellation; what it does not prove is that a control intent is
// what causes that cancellation to happen at all).
//
// Wiring: ControlLoop.StopProcess is a closure that cancels a real,
// running ProcessSupervisor's context and waits for ProcessSupervisor.Run
// to actually return, so ControlLoop only reports "terminated" once the
// real supervisor has genuinely finished tearing down the group. This is
// exactly the measured gap dp-v2-019 d3 records: grep over all non-test
// *.go files finds ControlLoop referenced nowhere outside its own test
// file, so StopProcess has only ever been a fake closure returning a
// canned error; here it is wired to something real for the first time.
//
// The child and grandchild are a real OS process group (a shell script run
// by ProcessSupervisor, reusing supervisor_test.go's own grandchildScript
// helper unmodified) that both trap SIGTERM by writing a marker file and
// otherwise keep looping, forcing the supervisor to escalate to SIGKILL
// after TermGrace. This test asserts, from real observations only (no
// fixed sleep, no wall-clock upper bound): both marker files exist (TERM
// was delivered to the whole group before anything died), the elapsed time
// for StopProcess is at least TermGrace (KILL could not have been sent
// before TERM's grace period elapsed -- a deterministic lower bound, not a
// flaky one), both the child and the grandchild are actually gone
// (`waitUntilProcessGone`, polled under a bounded deadline), and the
// journal's control_observation for this revision has state "terminated".
func TestControlIntentKillsRealProcessGroupThroughControlLoop(t *testing.T) {
	dir := t.TempDir()
	script := grandchildScript(dir)
	const termGrace = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() {
		runErr <- (ProcessSupervisor{TermGrace: termGrace}).Run(ctx, []string{"sh", "-c", script})
	}()

	waitForPath(t, filepath.Join(dir, "child_ready"), 5*time.Second)
	waitForPath(t, filepath.Join(dir, "grandchild_ready"), 5*time.Second)
	pidBytes, err := os.ReadFile(filepath.Join(dir, "grandchild.pid"))
	if err != nil {
		t.Fatal(err)
	}
	grandchildPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}

	journalDir := t.TempDir()
	journal, err := OpenJournal(journalDir)
	if err != nil {
		t.Fatal(err)
	}

	// stopCalled/stopErr let the assertions below inspect exactly what the
	// real supervisor reported, independent of what ControlLoop.observe
	// then wrote to the journal.
	var stopErr error
	loop := &ControlLoop{
		Journal: journal,
		StopProcess: func(context.Context) error {
			cancel()
			select {
			case err := <-runErr:
				// ProcessSupervisor.Run returns ctx.Err() (context.Canceled)
				// on a successful cancellation-driven kill; that is success
				// here, not failure. Any other error is a genuine failure to
				// report to the control loop as "unknown".
				if err != nil && !errors.Is(err, context.Canceled) {
					stopErr = err
					return err
				}
				return nil
			case <-time.After(15 * time.Second):
				stopErr = errors.New("process group did not report termination in time")
				return stopErr
			}
		},
	}

	start := time.Now()
	if err := loop.Apply(context.Background(), domain.ControlIntent{Revision: 1, Mode: domain.ControlImmediateStop}); err != nil {
		t.Fatalf("ControlLoop.Apply: %v", err)
	}
	elapsed := time.Since(start)
	if stopErr != nil {
		t.Fatalf("StopProcess reported an error: %v", stopErr)
	}

	// TERM was delivered to the whole group (both markers exist) before
	// anything died: the child and grandchild only write these markers
	// inside their own TERM traps, and both then keep looping, so the
	// supervisor could only have finished (elapsed >= termGrace) by having
	// waited out the grace period and then sent KILL.
	waitForPath(t, filepath.Join(dir, "child_term"), 2*time.Second)
	waitForPath(t, filepath.Join(dir, "grandchild_term"), 2*time.Second)
	if elapsed < termGrace {
		t.Fatalf("StopProcess returned after %s, less than TermGrace=%s: KILL could not have waited for TERM's grace period", elapsed, termGrace)
	}

	// Both the child (whose pid was the process group leader started by
	// ProcessSupervisor -- reaped by cmd.Wait() inside Run, confirmed by
	// runErr already having fired above) and the grandchild are actually
	// gone, not merely signalled.
	waitUntilProcessGone(t, grandchildPID, 5*time.Second)

	events, err := journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	var sawTerminatedObservation bool
	for _, event := range events {
		if event.Kind != "control_observation" {
			continue
		}
		if strings.Contains(string(event.Payload), `"state":"terminated"`) {
			sawTerminatedObservation = true
		}
	}
	if !sawTerminatedObservation {
		t.Fatalf("journal has no terminated control_observation: events=%#v", events)
	}
}
