package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// baseTime is the origin of every injected clock in this package's tests.
// No test reads the wall clock, sleeps, starts a timer or starts a goroutine.
var baseTime = time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return baseTime.Add(time.Duration(minutes) * time.Minute) }

// digestOf produces a 64-hex value from a label, so no test carries a
// high-entropy literal and every fixture digest is a function of its name.
func digestOf(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

func candidateOf(version string) string { return "cand-" + version }

// fixture is one machine: a root, a signer whose public half is the only
// entry in the machine's trust anchor, and the anchor written at the fixed
// path with the canonical 0400 mode.
type fixture struct {
	root    string
	keyID   string
	signer  ed25519.PrivateKey
	trusted ed25519.PublicKey
	anchors AnchorSet
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{root: t.TempDir(), keyID: "release-2026-08", signer: priv, trusted: pub}
	f.anchors = NewAnchorSet(Ed25519Anchor(f.keyID, pub))
	writeAnchorFile(t, f.root, FormatAnchorLine(Ed25519Anchor(f.keyID, pub)), 0o400)
	return f
}

// writeAnchorFile places an anchor at the fixed path with an explicit mode.
func writeAnchorFile(t *testing.T, root, content string, mode fs.FileMode) {
	t.Helper()
	path := AnchorPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// resolver is the fixture's production-shaped resolver: the fixed path under
// the fixture root, checked against the real invoking uid.
func (f *fixture) resolver() AnchorResolver {
	return AnchorResolver{Root: f.root, InvokingUID: os.Getuid()}
}

// manifestFor is the complete v2 manifest for one version, before any
// mutation a negative case wants to apply.
func (f *fixture) manifestFor(version string, binary []byte) Manifest {
	digest := sha256.Sum256(binary)
	return Manifest{
		Schema:          ManifestSchema,
		Version:         version,
		OS:              runtime.GOOS,
		Architecture:    runtime.GOARCH,
		BinarySHA256:    hex.EncodeToString(digest[:]),
		SchemaMin:       1,
		SchemaMax:       2,
		SigningKeyID:    f.keyID,
		Algorithm:       AlgorithmEd25519,
		BundleDigest:    digestOf("bundle:" + version),
		CandidateID:     candidateOf(version),
		ContractRelease: "release-contract-2026.08",
		ContractDigest:  digestOf("contract:" + version),
		RunnerAPIMin:    1,
		RunnerAPIMax:    1,
	}
}

// sign renders a manifest and signs manifest || sha256(binary).
func (f *fixture) sign(t *testing.T, manifest Manifest, binary []byte) Bundle {
	t.Helper()
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(binary)
	signed := append(append([]byte(nil), encoded...), digest[:]...)
	return Bundle{Manifest: encoded, Binary: binary, Signature: ed25519.Sign(f.signer, signed)}
}

// bundle is a correctly signed bundle for version, with mutate applied to
// the manifest before signing so a negative case is still a *signed* bundle
// and the refusal under test is the one being measured.
func (f *fixture) bundle(t *testing.T, version string, binary []byte, mutate func(*Manifest)) Bundle {
	t.Helper()
	manifest := f.manifestFor(version, binary)
	if mutate != nil {
		mutate(&manifest)
	}
	return f.sign(t, manifest, binary)
}

func (f *fixture) install(t *testing.T, state *State, version string, binary []byte, now time.Time) Manifest {
	t.Helper()
	manifest, err := InstallRecorded(f.root, state, f.bundle(t, version, binary, nil), f.anchors, now)
	if err != nil {
		t.Fatalf("install %s: %v", version, err)
	}
	return manifest
}

// promote drives a version onto preview and then onto stable, which is the
// only forward path stable accepts.
func (f *fixture) promote(t *testing.T, state *State, version string, now time.Time) {
	t.Helper()
	for _, channel := range []string{"preview", "stable"} {
		req := SwitchRequest{Channel: channel, Version: version, Direction: SwitchForward, Reason: "test promotion", CandidateDigest: candidateOf(version)}
		if err := Switch(f.root, state, req, now); err != nil {
			t.Fatalf("switch %s to %s: %v", channel, version, err)
		}
	}
}

func TestInstallSwitchAndRollbackKeepBothVersions(t *testing.T) {
	f := newFixture(t)
	state := NewState(1)
	f.install(t, state, "1.0.0", []byte("one"), at(0))
	f.promote(t, state, "1.0.0", at(1))
	f.install(t, state, "2.0.0-preview.1", []byte("two"), at(2))
	f.promote(t, state, "2.0.0-preview.1", at(3))

	if state.Stable != "2.0.0-preview.1" || state.PreviousStable != "1.0.0" {
		t.Fatalf("stable=%q previous=%q", state.Stable, state.PreviousStable)
	}
	rollback := SwitchRequest{Channel: "stable", Version: "1.0.0", Direction: SwitchRollback, Reason: "the successor failed its exercise"}
	if err := Switch(f.root, state, rollback, at(4)); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(f.root, "stable"))
	if err != nil || target != filepath.Join("versions", "1.0.0") {
		t.Fatalf("target=%q err=%v", target, err)
	}
	for _, version := range []string{"1.0.0", "2.0.0-preview.1"} {
		if _, err := os.Stat(filepath.Join(VersionDir(f.root, version), "runner")); err != nil {
			t.Fatalf("rollback deleted %s: %v", version, err)
		}
	}
	// The record survives a reload, because every switch was written after
	// the symlink rename it describes.
	reloaded, err := LoadState(f.root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Switches) != 5 || reloaded.Stable != "1.0.0" {
		t.Fatalf("switches=%d stable=%q", len(reloaded.Switches), reloaded.Stable)
	}
}

func TestVerifyRejectsTamperAndIncompatibleSchema(t *testing.T) {
	f := newFixture(t)
	bundle := f.bundle(t, "1.0.0", []byte("binary"), nil)
	bundle.Binary[0] = 'X'
	if _, err := Verify(bundle, f.anchors, 1); err == nil {
		t.Fatal("tamper accepted")
	}
	bundle = f.bundle(t, "1.0.0", []byte("binary"), nil)
	if _, err := Verify(bundle, f.anchors, 3); err == nil {
		t.Fatal("incompatible schema accepted")
	}
}
