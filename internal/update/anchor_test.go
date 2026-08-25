package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// countingLauncher builds a launcher whose confinement probe and process
// starter only count calls, so a refusal can be shown to have started no
// child rather than merely to have returned an error.
type callCounts struct {
	probes int
	starts int
}

func (c *callCounts) launcher(root string, anchors AnchorResolver) Launcher {
	return Launcher{
		Root:            root,
		CanonicalSchema: 1,
		Anchors:         anchors,
		Probe: func(context.Context) error {
			c.probes++
			return nil
		},
		Start: func(context.Context, ChildSpec) (ChildResult, error) {
			c.starts++
			return ChildResult{}, nil
		},
	}
}

func validAnchorContent(t *testing.T, keyID string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return FormatAnchorLine(Ed25519Anchor(keyID, pub))
}

// TestTrustAnchorRefusalsAreEnumeratedAndFailClosed walks the six refusals
// of docs/operations/self-update.md section 3.2 in order, plus the seventh
// this implementation adds for a line that does not parse. Each case asserts
// three things: the refusal is the expected one, it unwraps to
// ErrAnchorUnavailable, and a launcher using that anchor touches no
// versions/ directory, runs no confinement probe and starts no child.
func TestTrustAnchorRefusalsAreEnumeratedAndFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		refusal AnchorRefusal
		prepare func(t *testing.T, root string) AnchorResolver
	}{
		{
			name:    "1-absent-at-the-fixed-path",
			refusal: AnchorAbsent,
			prepare: func(t *testing.T, root string) AnchorResolver {
				return AnchorResolver{Root: root, InvokingUID: os.Getuid()}
			},
		},
		{
			name:    "2a-not-a-regular-file-directory",
			refusal: AnchorNotRegular,
			prepare: func(t *testing.T, root string) AnchorResolver {
				if err := os.MkdirAll(AnchorPath(root), 0o700); err != nil {
					t.Fatal(err)
				}
				return AnchorResolver{Root: root, InvokingUID: os.Getuid()}
			},
		},
		{
			name:    "2b-not-a-regular-file-symlink",
			refusal: AnchorNotRegular,
			prepare: func(t *testing.T, root string) AnchorResolver {
				elsewhere := filepath.Join(root, "elsewhere")
				if err := os.MkdirAll(elsewhere, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(AnchorPath(root)), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(elsewhere, AnchorPath(root)); err != nil {
					t.Fatal(err)
				}
				return AnchorResolver{Root: root, InvokingUID: os.Getuid()}
			},
		},
		{
			name:    "3-not-owned-by-the-invoking-user",
			refusal: AnchorForeignOwner,
			prepare: func(t *testing.T, root string) AnchorResolver {
				writeAnchorFile(t, root, validAnchorContent(t, "release-2026-08"), 0o400)
				// The invoking uid is injected, so the comparison
				// production performs against os.Getuid() is driven here
				// without needing a second real uid.
				return AnchorResolver{Root: root, InvokingUID: os.Getuid() + 1}
			},
		},
		{
			name:    "4-mode-wider-than-0600",
			refusal: AnchorModeTooWide,
			prepare: func(t *testing.T, root string) AnchorResolver {
				writeAnchorFile(t, root, validAnchorContent(t, "release-2026-08"), 0o640)
				return AnchorResolver{Root: root, InvokingUID: os.Getuid()}
			},
		},
		{
			name:    "4b-execute-bit-set",
			refusal: AnchorModeTooWide,
			prepare: func(t *testing.T, root string) AnchorResolver {
				writeAnchorFile(t, root, validAnchorContent(t, "release-2026-08"), 0o500)
				return AnchorResolver{Root: root, InvokingUID: os.Getuid()}
			},
		},
		{
			name:    "5-empty",
			refusal: AnchorEmpty,
			prepare: func(t *testing.T, root string) AnchorResolver {
				writeAnchorFile(t, root, "", 0o400)
				return AnchorResolver{Root: root, InvokingUID: os.Getuid()}
			},
		},
		{
			name:    "6-zero-accepted-entries",
			refusal: AnchorNoEntries,
			prepare: func(t *testing.T, root string) AnchorResolver {
				writeAnchorFile(t, root, "# replicated out of band\n\n   \n", 0o400)
				return AnchorResolver{Root: root, InvokingUID: os.Getuid()}
			},
		},
		{
			name:    "7-malformed-entry",
			refusal: AnchorMalformed,
			prepare: func(t *testing.T, root string) AnchorResolver {
				writeAnchorFile(t, root, "release-2026-08 "+AlgorithmEd25519+"\n", 0o400)
				return AnchorResolver{Root: root, InvokingUID: os.Getuid()}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			resolver := tc.prepare(t, root)
			set, err := resolver.Resolve()
			if err == nil {
				t.Fatalf("anchor accepted with %d entries; want refusal %s", set.Len(), tc.refusal)
			}
			if !errors.Is(err, ErrAnchorUnavailable) {
				t.Fatalf("err = %v, want it to unwrap to ErrAnchorUnavailable", err)
			}
			var refusal *AnchorError
			if !errors.As(err, &refusal) || refusal.Refusal != tc.refusal {
				t.Fatalf("err = %v, want refusal %s", err, tc.refusal)
			}
			t.Logf("verdict %s: refused with %s", tc.name, refusal.Refusal)

			counts := &callCounts{}
			if _, err := counts.launcher(root, resolver).Launch(context.Background(), "stable"); !errors.Is(err, ErrAnchorUnavailable) {
				t.Fatalf("launcher err = %v, want the same anchor refusal", err)
			}
			if counts.probes != 0 || counts.starts != 0 {
				t.Fatalf("probes=%d starts=%d, want a refusal before either", counts.probes, counts.starts)
			}
			if _, err := os.Stat(filepath.Join(root, "versions")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("versions/ exists after an anchor refusal: %v", err)
			}
		})
	}
}

// TestTrustAnchorIsASetAndSelectsByKeyID is the positive half: a canonical
// 0400 anchor resolves, an anchor holding two entries verifies bundles
// signed by either (which is what makes rotation add-then-remove rather
// than a flag day), and the manifest-side refusals fire on a key id the
// anchor does not hold, on an algorithm the entry does not declare, and on
// an entry whose algorithm this build cannot verify.
func TestTrustAnchorIsASetAndSelectsByKeyID(t *testing.T) {
	f := newFixture(t)
	set, err := f.resolver().Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if set.Len() != 1 || set.KeyIDs()[0] != f.keyID {
		t.Fatalf("entries=%v", set.KeyIDs())
	}
	// A 0600 anchor is inside the mode bound; 0400 is the canonical mode.
	writeAnchorFile(t, f.root, FormatAnchorLine(Ed25519Anchor(f.keyID, f.trusted)), 0o600)
	if _, err := f.resolver().Resolve(); err != nil {
		t.Fatalf("mode 0600 refused: %v", err)
	}

	// Rotation overlap: the outgoing and incoming identities coexist.
	nextPub, nextPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	content := FormatAnchorLine(Ed25519Anchor(f.keyID, f.trusted)) + FormatAnchorLine(Ed25519Anchor("release-2026-09", nextPub))
	writeAnchorFile(t, f.root, content, 0o400)
	rotated, err := f.resolver().Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Len() != 2 {
		t.Fatalf("entries=%v, want both the outgoing and the incoming identity", rotated.KeyIDs())
	}
	if _, err := Verify(f.bundle(t, "1.0.0", []byte("one"), nil), rotated, 1); err != nil {
		t.Fatalf("outgoing identity refused during overlap: %v", err)
	}
	incoming := &fixture{root: f.root, keyID: "release-2026-09", signer: nextPriv, trusted: nextPub}
	if _, err := Verify(incoming.bundle(t, "1.0.0", []byte("one"), nil), rotated, 1); err != nil {
		t.Fatalf("incoming identity refused during overlap: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"key-id-absent-from-the-anchor", func(m *Manifest) { m.SigningKeyID = "release-1999-01" }},
		{"key-id-empty", func(m *Manifest) { m.SigningKeyID = "" }},
		{"algorithm-the-entry-does-not-declare", func(m *Manifest) { m.Algorithm = "ecdsa-p256" }},
		{"algorithm-empty", func(m *Manifest) { m.Algorithm = "" }},
	} {
		if _, err := Verify(f.bundle(t, "1.0.0", []byte("one"), tc.mutate), rotated, 1); err == nil {
			t.Fatalf("%s: accepted", tc.name)
		} else {
			t.Logf("verdict %s: %v", tc.name, err)
		}
	}

	// An entry whose algorithm this build cannot verify stays in the set --
	// that is what lets a non-Ed25519 identity be added during a signer
	// migration -- and is refused at verification time rather than being
	// silently treated as Ed25519.
	future := NewAnchorSet(AnchorEntry{KeyID: f.keyID, Algorithm: "kms-unknown-2027"})
	if _, err := Verify(f.bundle(t, "1.0.0", []byte("one"), func(m *Manifest) { m.Algorithm = "kms-unknown-2027" }), future, 1); err == nil {
		t.Fatal("an entry with an unverifiable algorithm was used to verify a bundle")
	}
	if _, err := Verify(f.bundle(t, "1.0.0", []byte("one"), nil), NewAnchorSet(), 1); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("an empty anchor set verified a bundle")
	}
}
