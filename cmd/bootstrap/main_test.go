package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/update"
)

// TestMain doubles as the helper child process, so the process-group test
// executes a real binary without needing a shell, a fixture binary or any
// tool from PATH.
func TestMain(m *testing.M) {
	if os.Getenv("BOOTSTRAP_HELPER_CHILD") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type machine struct {
	root    string
	bundles string
	signer  ed25519.PrivateKey
	keyID   string
}

func newMachine(t *testing.T) *machine {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m := &machine{root: t.TempDir(), bundles: t.TempDir(), signer: priv, keyID: "release-2026-08"}
	anchorPath := update.AnchorPath(m.root)
	if err := os.MkdirAll(filepath.Dir(anchorPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchorPath, []byte(update.FormatAnchorLine(update.Ed25519Anchor(m.keyID, pub))), 0o400); err != nil {
		t.Fatal(err)
	}
	return m
}

// writeBundle lays a signed bundle out as three files, the way a release
// artifact would arrive on a machine. signer chooses who signs it, which is
// how the substituted-key case is expressed.
func (m *machine) writeBundle(t *testing.T, version string, signer ed25519.PrivateKey) (string, string, string) {
	t.Helper()
	binary := []byte("runner-" + version)
	digest := sha256.Sum256(binary)
	manifest := update.Manifest{
		Schema:          update.ManifestSchema,
		Version:         version,
		OS:              runtime.GOOS,
		Architecture:    runtime.GOARCH,
		BinarySHA256:    hex.EncodeToString(digest[:]),
		SchemaMin:       1,
		SchemaMax:       2,
		SigningKeyID:    m.keyID,
		Algorithm:       update.AlgorithmEd25519,
		BundleDigest:    sha256Hex("bundle:" + version),
		CandidateID:     "cand-" + version,
		ContractRelease: "release-contract-2026.08",
		ContractDigest:  sha256Hex("contract:" + version),
		RunnerAPIMin:    1,
		RunnerAPIMax:    1,
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(m.bundles, version)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	binaryPath := filepath.Join(dir, "runner")
	signaturePath := filepath.Join(dir, "signature")
	signed := append(append([]byte(nil), encoded...), digest[:]...)
	for path, data := range map[string][]byte{manifestPath: encoded, binaryPath: binary, signaturePath: ed25519.Sign(signer, signed)} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return manifestPath, binaryPath, signaturePath
}

func sha256Hex(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

func (m *machine) installArgs(version, manifestPath, binaryPath, signaturePath string) []string {
	return []string{"install", "--root", m.root, "--manifest", manifestPath, "--binary", binaryPath, "--signature", signaturePath, "--schema", "1"}
}

// TestCLIResolvesItsOwnTrustAnchorAndHasNoPublicKeyFlag is the measured
// defect closed at the command boundary: the Bootstrapper no longer accepts
// a trust anchor from its caller, so a caller who can sign with a key of
// their own can no longer install anything.
func TestCLIResolvesItsOwnTrustAnchorAndHasNoPublicKeyFlag(t *testing.T) {
	m := newMachine(t)
	manifestPath, binaryPath, signaturePath := m.writeBundle(t, "1.0.0", m.signer)
	var out bytes.Buffer

	// The flag is gone, so a caller cannot even name a key.
	withFlag := append(m.installArgs("1.0.0", manifestPath, binaryPath, signaturePath), "--public-key", "/tmp/attacker.pub")
	if err := run(context.Background(), withFlag, &out); err == nil {
		t.Fatal("--public-key was accepted")
	} else {
		t.Logf("verdict --public-key: %v", err)
	}

	// A bundle signed by a key the machine's anchor does not hold is refused,
	// even though it is a perfectly valid signature over a perfectly valid
	// manifest.
	_, attacker, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	foreignManifest, foreignBinary, foreignSignature := m.writeBundle(t, "9.9.9", attacker)
	if err := run(context.Background(), m.installArgs("9.9.9", foreignManifest, foreignBinary, foreignSignature), &out); err == nil {
		t.Fatal("a bundle signed by a substituted key was installed")
	} else {
		t.Logf("verdict substituted-key: %v", err)
	}
	if _, err := os.Stat(update.VersionDir(m.root, "9.9.9")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused install wrote into versions/: %v", err)
	}

	// The legitimate bundle installs.
	out.Reset()
	if err := run(context.Background(), m.installArgs("1.0.0", manifestPath, binaryPath, signaturePath), &out); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "1.0.0" {
		t.Fatalf("stdout = %q", out.String())
	}

	// With no anchor at the fixed path, every verb that needs one refuses
	// before touching versions/ and before starting a child.
	if err := os.Remove(update.AnchorPath(m.root)); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		m.installArgs("1.0.0", manifestPath, binaryPath, signaturePath),
		{"run", "--root", m.root, "--channel", "stable", "--schema", "1"},
	} {
		err := run(context.Background(), args, &out)
		if !errors.Is(err, update.ErrAnchorUnavailable) {
			t.Fatalf("%v: err = %v, want the anchor refusal", args[0], err)
		}
		t.Logf("verdict %s without an anchor: %v", args[0], err)
	}

	if err := run(context.Background(), []string{"bogus"}, &out); err == nil {
		t.Fatal("an unknown verb was accepted")
	}
	if err := run(context.Background(), nil, &out); err == nil {
		t.Fatal("an empty argv was accepted")
	}
}

// TestCLISwitchIsRecordedAndRefusesConsecutiveRollbacks drives the recorded,
// monotonic switch through the command boundary.
func TestCLISwitchIsRecordedAndRefusesConsecutiveRollbacks(t *testing.T) {
	m := newMachine(t)
	var out bytes.Buffer
	for _, version := range []string{"1.0.0", "2.0.0"} {
		manifestPath, binaryPath, signaturePath := m.writeBundle(t, version, m.signer)
		if err := run(context.Background(), m.installArgs(version, manifestPath, binaryPath, signaturePath), &out); err != nil {
			t.Fatal(err)
		}
	}
	sw := func(channel, version, direction, reason, candidate string) error {
		return run(context.Background(), []string{"switch", "--root", m.root, "--channel", channel, "--version", version, "--direction", direction, "--reason", reason, "--candidate", candidate, "--schema", "1"}, &out)
	}
	if err := sw("preview", "1.0.0", "forward", "stage", "cand-1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := sw("stable", "1.0.0", "forward", "adopt", "cand-1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := sw("preview", "2.0.0", "forward", "stage", "cand-2.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := sw("stable", "2.0.0", "forward", "promote", "cand-2.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := sw("stable", "1.0.0", "rollback", "regression", ""); err != nil {
		t.Fatal(err)
	}
	err := sw("stable", "2.0.0", "rollback", "ping-pong", "")
	if !errors.Is(err, update.ErrRollbackExhausted) {
		t.Fatalf("second consecutive rollback err = %v", err)
	}
	t.Logf("verdict: %v", err)
	if err := sw("stable", "1.0.0", "forward", "no reason given", ""); err == nil {
		t.Fatal("a forward move with no candidate was accepted")
	}

	state, err := update.LoadState(m.root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Switches) != 5 || state.Stable != "1.0.0" {
		t.Fatalf("switches=%d stable=%q", len(state.Switches), state.Stable)
	}
	target, err := os.Readlink(filepath.Join(m.root, "stable"))
	if err != nil || target != filepath.Join("versions", "1.0.0") {
		t.Fatalf("stable -> %q (%v)", target, err)
	}
	t.Logf("verdict: %d switches recorded, stable -> %s", len(state.Switches), target)
}

// TestChildStartsInItsOwnProcessGroup executes a real child through the
// production process starter and reads its process group from the kernel. It
// is the reason the launcher survives a signal aimed at the child's group and
// can therefore re-verify and re-launch after the child exits.
func TestChildStartsInItsOwnProcessGroup(t *testing.T) {
	t.Setenv("BOOTSTRAP_HELPER_CHILD", "1")
	parentGroup, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		ownGroup bool
	}{{"own-process-group", true}, {"inherited-process-group", false}} {
		t.Run(tc.name, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			var childGroup, childPID int
			starter := processStarter{
				stdin:  reader,
				stdout: io.Discard,
				stderr: io.Discard,
				afterStart: func(cmd *exec.Cmd) error {
					childPID = cmd.Process.Pid
					childGroup, err = syscall.Getpgid(childPID)
					if err != nil {
						return err
					}
					return writer.Close()
				},
			}
			result, err := starter.start(context.Background(), update.ChildSpec{Path: os.Args[0], NewProcessGroup: tc.ownGroup})
			if err != nil {
				t.Fatal(err)
			}
			if result.ExitCode != 0 {
				t.Fatalf("child exit code = %d", result.ExitCode)
			}
			if tc.ownGroup {
				if childGroup == parentGroup || childGroup != childPID {
					t.Fatalf("child pgid=%d pid=%d parent pgid=%d; want a new group led by the child", childGroup, childPID, parentGroup)
				}
			} else if childGroup != parentGroup {
				t.Fatalf("child pgid=%d, want the parent's %d", childGroup, parentGroup)
			}
			t.Logf("verdict %s: child pid=%d pgid=%d, launcher pgid=%d", tc.name, childPID, childGroup, parentGroup)
		})
	}
}
