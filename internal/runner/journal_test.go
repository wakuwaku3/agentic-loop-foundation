package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestJournalRecoveryAndIdempotentAppend(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(filepath.Join(dir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	e := JournalEvent{ID: "op-1", Kind: "checkpoint"}
	if err := j.Append(e); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(e); err != ErrJournalDuplicate {
		t.Fatalf("duplicate=%v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "journal", "events.log"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"id":"partial"`)
	_ = f.Close()
	j2, _ := OpenJournal(filepath.Join(dir, "journal"))
	events, err := j2.Replay()
	if err != nil || len(events) != 1 {
		t.Fatalf("replay=%d %v", len(events), err)
	}
}

func TestJournalRejectsCorruptCompleteLine(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "events.log"), []byte("{\"id\":\"broken\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJournal(dir); err != ErrJournalCorrupt {
		t.Fatalf("corruption accepted: %v", err)
	}
}

func TestWorkspaceAndGuard(t *testing.T) {
	base := t.TempDir()
	w, err := NewWorkspace(filepath.Join(base, "work"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Create("exec-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Path("../escape"); err == nil {
		t.Fatal("workspace escape accepted")
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(base, "work", "link")); err == nil {
		if _, err := w.Path("link"); err == nil {
			t.Fatal("symlink workspace accepted")
		}
	}
	if err := GuardCommand([]string{"provider", "--token=Bearer abcdefghijklmnop"}, nil); err == nil {
		t.Fatal("argv secret accepted")
	}
	if err := GuardCommand([]string{"provider"}, []string{"API_TOKEN=abc"}); err == nil {
		t.Fatal("secret env accepted")
	}
	if err := GuardEnvironment([]string{"PATH=/usr/bin", "X=Bearer abcdefghijklmnop"}, map[string]bool{"PATH": true, "X": true}); err == nil {
		t.Fatal("secret env value accepted")
	}
	if RedactLog("Authorization: Bearer abcdefghijklmnop") == "Authorization: Bearer abcdefghijklmnop" {
		t.Fatal("log was not redacted")
	}
}

func TestSyntheticAndFakeProvider(t *testing.T) {
	p := CodexSyntheticAdapter{}
	got, err := p.Run(context.Background(), ProviderRequest{OperationID: "op", Workspace: "/tmp/work"})
	if err != nil || !got.Succeeded {
		t.Fatalf("synthetic=%+v %v", got, err)
	}
	f := &FakeProvider{Result: ProviderResult{Succeeded: true}}
	_, _ = f.Run(context.Background(), ProviderRequest{OperationID: "op", Workspace: "/tmp/work"})
	if len(f.Calls) != 1 {
		t.Fatal("fake did not record call")
	}
}
