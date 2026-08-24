package runner

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const (
	// DefaultLogMaxBytes and DefaultLogMaxLines are the hard caps applied
	// when a caller does not choose narrower ones explicitly.
	DefaultLogMaxBytes int64 = 1 << 20
	DefaultLogMaxLines int   = 5000

	logTruncationMarker = "... [agentic-loop runner: bounded diagnostic log truncated]\n"
)

// BoundedLog is a per-execution diagnostic log sink. It is written under the
// data root, never under the workspace, so the child process the log is
// diagnosing cannot read or tamper with it. Every write passes through
// RedactLog first, and the file is capped by both a hard byte count and a
// hard line count; once either cap is reached, an explicit truncation marker
// is written exactly once and every later write is silently dropped.
type BoundedLog struct {
	mu        sync.Mutex
	path      string
	maxBytes  int64
	maxLines  int
	bytes     int64
	lines     int
	truncated bool
}

// NewBoundedLog creates (or reopens) the bounded log file for executionID
// under dataRoot/logs, as an 0600 file. dataRoot must be absolute and must
// not be a workspace root.
func NewBoundedLog(dataRoot, executionID string, maxBytes int64, maxLines int) (*BoundedLog, error) {
	if dataRoot == "" || !filepath.IsAbs(dataRoot) {
		return nil, errors.New("bounded log data root must be absolute")
	}
	if executionID == "" || filepath.Base(executionID) != executionID || executionID == "." || executionID == ".." {
		return nil, errors.New("invalid execution id for bounded log")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultLogMaxBytes
	}
	if maxLines <= 0 {
		maxLines = DefaultLogMaxLines
	}
	dir := filepath.Join(dataRoot, "logs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, executionID+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	return &BoundedLog{path: path, maxBytes: maxBytes, maxLines: maxLines}, nil
}

// Path returns the absolute path of the underlying log file.
func (l *BoundedLog) Path() string { return l.path }

// Write redacts value with RedactLog, appends it as one line, and enforces
// the hard byte and line caps. Once either cap would be exceeded, the
// truncation marker is written once instead, and every subsequent call is a
// silent no-op.
func (l *BoundedLog) Write(value string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.truncated {
		return nil
	}
	redacted := RedactLog(value)
	line := redacted
	if len(line) == 0 || line[len(line)-1] != '\n' {
		line += "\n"
	}
	if l.lines+1 > l.maxLines || l.bytes+int64(len(line)) > l.maxBytes {
		return l.writeTruncationMarkerLocked()
	}
	if err := l.appendLocked([]byte(line)); err != nil {
		return err
	}
	l.bytes += int64(len(line))
	l.lines++
	return nil
}

func (l *BoundedLog) writeTruncationMarkerLocked() error {
	marker := []byte(logTruncationMarker)
	if err := l.appendLocked(marker); err != nil {
		return err
	}
	l.bytes += int64(len(marker))
	l.lines++
	l.truncated = true
	return nil
}

func (l *BoundedLog) appendLocked(b []byte) error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return err
}
