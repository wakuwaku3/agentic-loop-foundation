package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func signedBundle(t *testing.T, version string, binary []byte) (Bundle, ed25519.PublicKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(binary)
	manifest, err := json.Marshal(Manifest{Schema: ManifestSchema, Version: version, OS: runtime.GOOS, Architecture: runtime.GOARCH, BinarySHA256: hex.EncodeToString(digest[:]), SchemaMin: 1, SchemaMax: 2})
	if err != nil {
		t.Fatal(err)
	}
	signed := append(append([]byte(nil), manifest...), digest[:]...)
	return Bundle{Manifest: manifest, Binary: binary, Signature: ed25519.Sign(privateKey, signed)}, publicKey
}

func TestInstallSwitchAndRollbackKeepBothVersions(t *testing.T) {
	root := t.TempDir()
	one, key := signedBundle(t, "1.0.0", []byte("one"))
	if _, err := Install(root, one, key, 1); err != nil {
		t.Fatal(err)
	}
	if err := Switch(root, "stable", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	two, key2 := signedBundle(t, "2.0.0-preview.1", []byte("two"))
	if _, err := Install(root, two, key2, 2); err != nil {
		t.Fatal(err)
	}
	if err := Switch(root, "preview", "2.0.0-preview.1"); err != nil {
		t.Fatal(err)
	}
	if err := Switch(root, "preview", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(root, "preview"))
	if err != nil || target != filepath.Join("versions", "1.0.0") {
		t.Fatalf("target=%q err=%v", target, err)
	}
	if _, err := os.Stat(filepath.Join(root, "versions", "2.0.0-preview.1", "runner")); err != nil {
		t.Fatal("rollback deleted preview version")
	}
}

func TestVerifyRejectsTamperAndIncompatibleSchema(t *testing.T) {
	bundle, key := signedBundle(t, "1.0.0", []byte("binary"))
	bundle.Binary[0] = 'X'
	if _, err := Verify(bundle, key, 1); err == nil {
		t.Fatal("tamper accepted")
	}
	bundle, key = signedBundle(t, "1.0.0", []byte("binary"))
	if _, err := Verify(bundle, key, 3); err == nil {
		t.Fatal("incompatible schema accepted")
	}
}
