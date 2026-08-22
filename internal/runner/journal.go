package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	MaxJournalEvents       = 10000
	MaxJournalBytes  int64 = 64 << 20
)

var (
	ErrJournalDuplicate = errors.New("journal event already recorded")
	ErrJournalCorrupt   = errors.New("journal contains corrupt complete event")
	ErrJournalLimit     = errors.New("journal hard limit exceeded")
)

type JournalEvent struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	CreatedAt time.Time       `json:"created_at"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}
type Journal struct {
	dir    string
	mu     sync.Mutex
	index  map[string]struct{}
	events int
	bytes  int64
}

func OpenJournal(dir string) (*Journal, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return nil, errors.New("journal directory must be absolute")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	j := &Journal{dir: dir, index: map[string]struct{}{}}
	if err := j.loadIndex(); err != nil {
		return nil, err
	}
	return j, nil
}
func (j *Journal) path() string { return filepath.Join(j.dir, "events.log") }
func (j *Journal) loadIndex() error {
	b, err := os.ReadFile(j.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if int64(len(b)) > MaxJournalBytes {
		return ErrJournalLimit
	}
	parts := bytes.Split(b, []byte{'\n'})
	partial := len(parts) > 0 && len(parts[len(parts)-1]) > 0
	if partial {
		parts = parts[:len(parts)-1]
	}
	for _, line := range parts {
		if len(line) == 0 {
			continue
		}
		var e JournalEvent
		if json.Unmarshal(line, &e) != nil || e.ID == "" || e.Kind == "" {
			return ErrJournalCorrupt
		}
		if _, ok := j.index[e.ID]; !ok {
			j.index[e.ID] = struct{}{}
			j.events++
		}
		if j.events > MaxJournalEvents {
			return ErrJournalLimit
		}
	}
	j.bytes = int64(len(b))
	return nil
}
func (j *Journal) Append(e JournalEvent) error {
	if e.ID == "" || e.Kind == "" {
		return errors.New("journal event id and kind are required")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, ok := j.index[e.ID]; ok {
		return ErrJournalDuplicate
	}
	if j.events >= MaxJournalEvents {
		return ErrJournalLimit
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if j.bytes+int64(len(b)) > MaxJournalBytes {
		return ErrJournalLimit
	}
	f, err := os.OpenFile(j.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	j.index[e.ID] = struct{}{}
	j.events++
	j.bytes += int64(len(b))
	return nil
}
func (j *Journal) Replay() ([]JournalEvent, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	b, err := os.ReadFile(j.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(b, []byte{'\n'})
	if len(parts) > 0 && len(parts[len(parts)-1]) > 0 {
		parts = parts[:len(parts)-1]
	}
	out := make([]JournalEvent, 0, len(parts))
	for _, line := range parts {
		if len(line) == 0 {
			continue
		}
		var e JournalEvent
		if json.Unmarshal(line, &e) != nil || e.ID == "" || e.Kind == "" {
			return nil, ErrJournalCorrupt
		}
		out = append(out, e)
	}
	return out, nil
}
func (j *Journal) Snapshot(events []JournalEvent) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	tmp, err := os.CreateTemp(j.dir, ".events-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	_ = tmp.Chmod(0600)
	for _, e := range events {
		b, eErr := json.Marshal(e)
		if eErr != nil {
			_ = tmp.Close()
			return eErr
		}
		if _, err = tmp.Write(append(b, '\n')); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err = tmp.Sync(); err == nil {
		err = tmp.Close()
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, j.path()); err != nil {
		return err
	}
	j.index = map[string]struct{}{}
	j.events = 0
	j.bytes = 0
	return j.loadIndex()
}
