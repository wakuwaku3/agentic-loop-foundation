package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// waitForPath polls for path to exist, with a bounded deadline. It never
// sleeps a fixed amount and never t.Fatal's on the caller's behalf outside
// the deadline path, so callers can compose it.
func waitForPath(t *testing.T, path string, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitUntilProcessGone polls (bounded deadline) until signalling pid with 0
// fails, i.e. the process is no longer schedulable (dead or already reaped).
func waitUntilProcessGone(t *testing.T, pid int, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for pid %d to exit", pid)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// grandchildScript makes both the direct child and a grandchild it spawns
// (in the same, new process group, since ProcessSupervisor sets Setpgid on
// the leader) trap TERM by writing a marker file and otherwise keep looping,
// so the supervisor is forced to escalate to KILL. Readiness and the
// grandchild's own pid are also written as files so the test can synchronise
// on observable state instead of a fixed sleep.
func grandchildScript(dir string) string {
	return fmt.Sprintf(`
trap 'touch %[1]s/child_term' TERM
(
  trap 'touch %[1]s/grandchild_term' TERM
  echo $$ > %[1]s/grandchild.pid
  touch %[1]s/grandchild_ready
  while true; do sleep 1; done
) &
touch %[1]s/child_ready
while true; do sleep 1; done
`, dir)
}

func TestSupervisorCancelsProcessGroupTERMThenKILL(t *testing.T) {
	dir := t.TempDir()
	script := grandchildScript(dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() {
		runErr <- (ProcessSupervisor{TermGrace: 200 * time.Millisecond}).Run(ctx, []string{"sh", "-c", script})
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

	cancel()

	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("expected cancellation error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not return after cancellation")
	}

	// Both the child and the grandchild received TERM (proving the whole
	// process group was signalled, not just the direct child), and both
	// needed KILL to actually die because they kept looping after TERM.
	waitForPath(t, filepath.Join(dir, "child_term"), 2*time.Second)
	waitForPath(t, filepath.Join(dir, "grandchild_term"), 2*time.Second)
	waitUntilProcessGone(t, grandchildPID, 5*time.Second)
}
