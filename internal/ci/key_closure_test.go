package ci

import (
	"os"
	"path/filepath"
	"testing"
)

// TestKeyClosureGoldenFile asserts the tracked ci/key-closure.json is exactly
// what the generator produces from ci/components.json (dp-v2-045 d10, A5). A
// stale golden file would change every evidence key, because the file is in
// the unconditional set.
func TestKeyClosureGoldenFile(t *testing.T) {
	root := filepath.Join("..", "..")
	m, err := Load(filepath.Join(root, "ci", "components.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := RenderKeyClosureDocument(BuildKeyClosureDocument(m))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "ci", "key-closure.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("ci/key-closure.json is stale: run `make key-closure`\n--- tracked (%d bytes) ---\n%s\n--- generated (%d bytes) ---\n%s",
			len(got), string(got), len(want), string(want))
	}
}

// TestKeyClosureDocumentCarriesNoKeyOrHash asserts the invariant that makes
// the golden file safe to hash into every key: it contains no evidence key
// and no hash of any kind, so it is not a fixed point.
func TestKeyClosureDocumentCarriesNoKeyOrHash(t *testing.T) {
	root := filepath.Join("..", "..")
	b, err := os.ReadFile(filepath.Join(root, "ci", "key-closure.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"evidence_key", "sha256", "digest", "hash"} {
		if containsFold(string(b), forbidden) {
			t.Fatalf("ci/key-closure.json must contain no %q", forbidden)
		}
	}
	// A 64-hex run would be a key or a digest whatever it is called.
	run := 0
	for _, r := range string(b) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			run++
			if run >= 64 {
				t.Fatal("ci/key-closure.json contains a 64-hex run, which would make it a fixed point")
			}
			continue
		}
		run = 0
	}
}

func containsFold(s, sub string) bool {
	if len(sub) == 0 || len(sub) > len(s) {
		return false
	}
	lower := func(r byte) byte {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		ok := true
		for j := 0; j < len(sub); j++ {
			if lower(s[i+j]) != lower(sub[j]) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// writeTree materialises the given repository-relative files under dir.
func writeTree(t *testing.T, dir string, contents map[string]string) []string {
	t.Helper()
	var files []string
	for rel, body := range contents {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		files = append(files, rel)
	}
	return files
}

func appendByte(t *testing.T, dir, rel string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	b, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, append(b, 'X'), 0644); err != nil {
		t.Fatal(err)
	}
}

func componentByID(t *testing.T, m Manifest, id string) Component {
	t.Helper()
	for _, c := range m.Components {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no component %s", id)
	return Component{}
}

// TestEvidenceKeySensitivity holds the three key-side positive controls
// (dp-v2-045 d9 PC4-PC6). All three run over EvidenceKeyFromFiles with an
// injected file list under t.TempDir(): no test mutates a tracked file and no
// test runs git.
func TestEvidenceKeySensitivity(t *testing.T) {
	// PC4: framing. The two trees hold the same bytes, split differently
	// between path and content ("a"+"bc" versus "ab"+"c"). One synthetic
	// component id is used for both so that the id contributes identically
	// and the framing is the only difference. The pre-v2 algorithm wrote path
	// bytes immediately followed by content bytes and gave both trees the
	// same key; the length-prefixed framing must not.
	t.Run("PC4_framing_path_content_split", func(t *testing.T) {
		m := Manifest{Version: 1, Components: []Component{{
			ID:    "f",
			Roots: []string{"a", "ab"},
			Check: Check{Runner: "make", Target: "component-tooling"},
		}}}
		c := m.Components[0]

		dirOne := t.TempDir()
		filesOne := writeTree(t, dirOne, map[string]string{"a": "bc"})
		keyOne, err := EvidenceKeyFromFiles(dirOne, filesOne, m, c)
		if err != nil {
			t.Fatal(err)
		}

		dirTwo := t.TempDir()
		filesTwo := writeTree(t, dirTwo, map[string]string{"ab": "c"})
		keyTwo, err := EvidenceKeyFromFiles(dirTwo, filesTwo, m, c)
		if err != nil {
			t.Fatal(err)
		}
		if keyOne == keyTwo {
			t.Fatalf("PC4: ('a','bc') and ('ab','c') must not hash equal, both gave %s", keyOne)
		}
	})

	// PC5: a dependency's bytes must move the consumer's key, and must leave
	// an unrelated component's key fixed. The second half is the non-vacuity
	// control: without it, a bug that hashed every file into every key would
	// satisfy the first half.
	t.Run("PC5_dependency_byte_moves_consumer_not_unrelated", func(t *testing.T) {
		_, m := realManifest(t)
		dir := t.TempDir()
		files := writeTree(t, dir, map[string]string{
			"internal/application/service.go": "package application\n",
			"internal/scheduler/scheduler.go": "package scheduler\n",
			"Makefile":                        "component-tooling:\n\t@true\n",
		})
		consumer := componentByID(t, m, "runner")
		unrelated := componentByID(t, m, "scheduler")

		beforeConsumer, err := EvidenceKeyFromFiles(dir, files, m, consumer)
		if err != nil {
			t.Fatal(err)
		}
		beforeUnrelated, err := EvidenceKeyFromFiles(dir, files, m, unrelated)
		if err != nil {
			t.Fatal(err)
		}
		appendByte(t, dir, "internal/application/service.go")
		afterConsumer, err := EvidenceKeyFromFiles(dir, files, m, consumer)
		if err != nil {
			t.Fatal(err)
		}
		afterUnrelated, err := EvidenceKeyFromFiles(dir, files, m, unrelated)
		if err != nil {
			t.Fatal(err)
		}
		if beforeConsumer == afterConsumer {
			t.Fatal("PC5: one byte appended to a dependency's source did not move the consumer's key")
		}
		if beforeUnrelated != afterUnrelated {
			t.Fatal("PC5: an unrelated component's key moved, so the key is not selective")
		}
	})

	// PC6: the Makefile carries every component's verification entrypoint, so
	// editing it must invalidate every component's recorded evidence.
	t.Run("PC6_makefile_byte_moves_every_key", func(t *testing.T) {
		_, m := realManifest(t)
		dir := t.TempDir()
		files := writeTree(t, dir, map[string]string{"Makefile": "component-tooling:\n\t@true\n"})
		before := map[string]string{}
		for _, c := range m.Components {
			k, err := EvidenceKeyFromFiles(dir, files, m, c)
			if err != nil {
				t.Fatal(err)
			}
			before[c.ID] = k
		}
		if len(before) != 22 {
			t.Fatalf("PC6: expected 22 components, got %d", len(before))
		}
		appendByte(t, dir, "Makefile")
		for _, c := range m.Components {
			k, err := EvidenceKeyFromFiles(dir, files, m, c)
			if err != nil {
				t.Fatal(err)
			}
			if k == before[c.ID] {
				t.Fatalf("PC6: component %s key did not move when Makefile changed", c.ID)
			}
		}
	})
}
