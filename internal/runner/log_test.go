package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBoundedLogTruncatesAtCap writes far more than the configured cap and
// asserts the resulting file is never larger than the cap plus the bounded
// truncation marker's own length -- the marker is written exactly once and
// every write after it is a silent no-op.
func TestBoundedLogTruncatesAtCap(t *testing.T) {
	dataRoot := t.TempDir()
	const cap = 200
	log, err := NewBoundedLog(dataRoot, "exec-cap", cap, 1000)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 20) // 21 bytes with the trailing newline Write adds.
	written := 0
	for written < 2*cap {
		if err := log.Write(line); err != nil {
			t.Fatal(err)
		}
		written += len(line) + 1
	}
	info, err := os.Stat(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > int64(cap)+int64(len(logTruncationMarker)) {
		t.Fatalf("log file grew to %d bytes, want <= cap(%d)+marker(%d)", info.Size(), cap, len(logTruncationMarker))
	}
	body, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), strings.TrimSuffix(logTruncationMarker, "\n")) {
		t.Fatalf("truncation marker missing from log body: %q", body)
	}
}

// TestBoundedLogTruncatesAtLineCap proves the line cap is independently
// enforced, not merely the byte cap: many very short lines must still stop
// at the line count and receive the marker.
func TestBoundedLogTruncatesAtLineCap(t *testing.T) {
	dataRoot := t.TempDir()
	const maxLines = 5
	log, err := NewBoundedLog(dataRoot, "exec-lines", 1<<20, maxLines)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxLines+10; i++ {
		if err := log.Write("line"); err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != maxLines+1 { // maxLines content lines + one marker line.
		t.Fatalf("got %d lines, want %d (maxLines + marker): %q", len(lines), maxLines+1, body)
	}
	if lines[len(lines)-1] != strings.TrimSuffix(logTruncationMarker, "\n") {
		t.Fatalf("last line is not the truncation marker: %q", lines[len(lines)-1])
	}
}

// TestBoundedLogRedactsSecretShapedOutput is the redaction assertion new for
// A9 (TestWorkspaceAndGuard's own RedactLog assertion is left exactly as it
// is). It also carries the positive control required by validation.md
// section 6: a non-secret value of the same length must survive unchanged,
// so the redaction is proven to be selective rather than a blanket wipe.
func TestBoundedLogRedactsSecretShapedOutput(t *testing.T) {
	dataRoot := t.TempDir()
	log, err := NewBoundedLog(dataRoot, "exec-redact", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	secretLine := "Authorization: Bearer abcdefghijklmnop"
	nonSecretLine := "Authorization: not-a-secret-shaped-value"
	if err := log.Write(secretLine); err != nil {
		t.Fatal(err)
	}
	if err := log.Write(nonSecretLine); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "abcdefghijklmnop") {
		t.Fatalf("secret-shaped value was written unredacted: %q", body)
	}
	if !strings.Contains(string(body), "[REDACTED]") {
		t.Fatalf("redaction marker missing from log body: %q", body)
	}
	if !strings.Contains(string(body), nonSecretLine) {
		t.Fatalf("positive control failed: non-secret value did not survive byte for byte: %q", body)
	}
}

// TestBoundedLogFileIsPerExecutionAndNotUnderWorkspace confirms the file is
// 0600, lives under dataRoot (not under any workspace root the caller might
// also create under the same dataRoot's sibling directory), and is named for
// its execution id.
func TestBoundedLogFileIsPerExecutionAndNotUnderWorkspace(t *testing.T) {
	dataRoot := t.TempDir()
	workspaceRoot := filepath.Join(dataRoot, "workspaces")
	if err := os.MkdirAll(workspaceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	log, err := NewBoundedLog(dataRoot, "exec-42", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("log file mode = %v, want 0600", info.Mode().Perm())
	}
	if strings.HasPrefix(log.Path(), workspaceRoot) {
		t.Fatalf("bounded log path %s is under the workspace root %s", log.Path(), workspaceRoot)
	}
	if filepath.Base(log.Path()) != "exec-42.log" {
		t.Fatalf("log file is not named for its execution id: %s", log.Path())
	}
}
