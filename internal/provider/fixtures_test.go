package provider_test

// Fixture provenance and the secret-free-by-measurement scan (V2-027 A7, A15).
//
// Every check below walks internal/provider/testdata rather than reading a
// hardcoded list, and fails outright on a zero-file walk, so a fixture added
// later is covered automatically and a broken walk cannot pass.

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

const manifestName = "manifest.json"

type manifestEntry struct {
	File                     string `json:"file"`
	Provider                 string `json:"provider"`
	CLIVersionDeclared       string `json:"cli_version_declared"`
	ShapeSource              string `json:"shape_source"`
	ObservedAt               string `json:"observed_at"`
	Reason                   string `json:"reason"`
	CorrespondingLiveExercse string `json:"corresponding_live_exercise"`
}

type fixtureManifest struct {
	SchemaVersion string          `json:"schema_version"`
	Kind          string          `json:"kind"`
	TaskID        string          `json:"task_id"`
	Note          string          `json:"note"`
	Entries       []manifestEntry `json:"entries"`
}

// walkFixtures returns every file under testdata except the manifest itself,
// discovered by walking the directory.
func walkFixtures(t *testing.T) []string {
	t.Helper()
	var files []string
	manifests := 0
	err := filepath.WalkDir("testdata", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Name() == manifestName {
			manifests++
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk testdata: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("the fixture walk found zero files; every assertion below would pass vacuously")
	}
	if manifests != 1 {
		t.Fatalf("the walk found %d files named %s, want exactly 1", manifests, manifestName)
	}
	sort.Strings(files)
	return files
}

func readManifest(t *testing.T) fixtureManifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", manifestName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest fixtureManifest
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(manifest.Entries) == 0 {
		t.Fatal("the manifest declares zero entries")
	}
	return manifest
}

// digestValue is the only non-empty form a fixture's output, result or
// summary field may take: the literal sha256, a colon, and 64 lowercase hex
// digits. Anything else would be provider text in a tracked file.
var digestValue = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// jsonKeyShape finds JSON object keys textually, so the scan also covers the
// deliberately malformed fixture, which no decoder will parse.
var jsonKeyShape = regexp.MustCompile(`"([^"\\]+)"\s*:`)

func TestEveryFixtureIsSecretFreeByMeasurement(t *testing.T) {
	files := walkFixtures(t)
	checkedSizes, checkedValues, checkedPatterns, checkedKeys := 0, 0, 0, 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		// (1) Size.
		if len(raw) >= provider.MaxFixtureBytes {
			t.Fatalf("%s is %d bytes, which is not smaller than provider.MaxFixtureBytes (%d)", path, len(raw), provider.MaxFixtureBytes)
		}
		checkedSizes++

		// (2) No output, result or summary value other than the empty string
		// or a sha256 digest.
		var decoded map[string]any
		if json.Unmarshal(raw, &decoded) == nil {
			for _, field := range []string{"output", "result", "summary"} {
				value, present := decoded[field]
				if !present {
					continue
				}
				text, isText := value.(string)
				if !isText {
					t.Fatalf("%s: %q is not a string", path, field)
				}
				checkedValues++
				if text != "" && !digestValue.MatchString(text) {
					t.Fatalf("%s: %q = %q, which is neither empty nor a sha256 digest; a tracked file may not carry provider output text", path, field, text)
				}
			}
		} else {
			// The malformed fixture. Its bytes are still scanned textually
			// below; it simply has no decodable fields to check here.
			for _, field := range []string{`"output"`, `"result"`, `"summary"`} {
				if strings.Contains(string(raw), field) {
					t.Fatalf("%s does not parse as JSON yet carries a %s key; its value cannot be checked and must not exist", path, field)
				}
			}
			checkedValues++
		}

		// (3) Neither the package's forbidden pattern nor its secret pattern,
		// asserted through the package's own predicates rather than a copy.
		if provider.ForbiddenPatternMatches(raw) {
			t.Fatalf("%s matches this package's forbidden vocabulary pattern", path)
		}
		if provider.SecretPatternMatches(raw) {
			t.Fatalf("%s matches this package's secret-shape pattern", path)
		}
		checkedPatterns++

		// (4) No JSON key matching the credential deny list.
		for _, match := range jsonKeyShape.FindAllStringSubmatch(string(raw), -1) {
			checkedKeys++
			if matchesProviderCredentialDenyList(match[1]) {
				t.Fatalf("%s carries the JSON key %q, which matches the credential deny list", path, match[1])
			}
		}
	}
	if checkedSizes == 0 || checkedPatterns == 0 || checkedKeys == 0 {
		t.Fatalf("a check ran zero times: sizes=%d values=%d patterns=%d keys=%d", checkedSizes, checkedValues, checkedPatterns, checkedKeys)
	}

	// Positive controls, so none of the four checks can pass because its
	// matcher is broken.
	if !digestValue.MatchString("sha256:"+strings.Repeat("a", 64)) || digestValue.MatchString("the provider said something") {
		t.Fatal("the digest-value matcher is broken")
	}
	if !provider.ForbiddenPatternMatches([]byte(`{"output":"raw prompt"}`)) {
		t.Fatal("the forbidden-pattern predicate does not match a known-bad value")
	}
	if !provider.SecretPatternMatches([]byte(`Bearer abcdefghijklmnopqrstuvwxyz012345`)) {
		t.Fatal("the secret-pattern predicate does not match a known-bad value")
	}
	if !matchesProviderCredentialDenyList("api_key") {
		t.Fatal("the credential deny list does not match a known-bad key")
	}
	t.Logf("walked %d fixture files: sizes=%d output-values=%d pattern-scans=%d json-keys=%d. No fixture was derived from any real Provider response", len(files), checkedSizes, checkedValues, checkedPatterns, checkedKeys)
}

func TestManifestIsABijectionWithTheFixtureFilesOnDisk(t *testing.T) {
	files := walkFixtures(t)
	manifest := readManifest(t)
	if manifest.SchemaVersion != "v1" || manifest.Kind != "provider-fixture-manifest" || manifest.TaskID != "V2-027" {
		t.Fatalf("manifest header = %#v", manifest)
	}

	onDisk := map[string]int{}
	for _, path := range files {
		onDisk[filepath.Base(path)]++
	}
	declared := map[string]int{}
	for _, entry := range manifest.Entries {
		declared[entry.File]++
	}
	for name, count := range onDisk {
		if declared[name] != 1 {
			t.Fatalf("%s has %d manifest entries, want exactly 1", name, declared[name])
		}
		if count != 1 {
			t.Fatalf("%s was walked %d times", name, count)
		}
	}
	for name, count := range declared {
		if count != 1 {
			t.Fatalf("the manifest declares %s %d times", name, count)
		}
		if onDisk[name] != 1 {
			t.Fatalf("the manifest declares %s, which the walk did not find on disk", name)
		}
	}
	if len(onDisk) != len(declared) {
		t.Fatalf("%d files on disk, %d manifest entries", len(onDisk), len(declared))
	}
	t.Logf("bijection over %d fixture files and %d manifest entries", len(onDisk), len(declared))
}

// measuredCLIVersions is what --version reported on this machine at
// implementation time. Reading a version consumes no Provider usage and needs
// no authentication.
var measuredCLIVersions = map[string]string{
	"codex":    "0.149.1",
	"claude":   "2.1.241",
	"opencode": "1.18.22",
}

func TestEveryManifestEntryDeclaresItsProvenanceHonestly(t *testing.T) {
	manifest := readManifest(t)
	perProvider := map[string]int{}
	for _, entry := range manifest.Entries {
		if !provider.IsProviderName(entry.Provider) {
			t.Fatalf("%s: provider = %q, which is not one of the three adapter names", entry.File, entry.Provider)
		}
		perProvider[entry.Provider]++
		if !strings.HasPrefix(entry.File, entry.Provider+"-") {
			t.Fatalf("%s does not belong to the provider it declares (%s)", entry.File, entry.Provider)
		}
		if entry.ShapeSource != "projected-shape-hand-authored" {
			t.Fatalf("%s: shape_source = %q; every fixture this task wrote is hand authored to the projected shape and none was captured from a real response", entry.File, entry.ShapeSource)
		}
		if entry.CorrespondingLiveExercse != "provider-live-multi" {
			t.Fatalf("%s: corresponding_live_exercise = %q, want provider-live-multi, which is the exercise that owns the real-CLI confirmation these shapes lack", entry.File, entry.CorrespondingLiveExercse)
		}
		if entry.CLIVersionDeclared != measuredCLIVersions[entry.Provider] {
			t.Fatalf("%s: cli_version_declared = %q, but --version measured %q for %s", entry.File, entry.CLIVersionDeclared, measuredCLIVersions[entry.Provider], entry.Provider)
		}
		if entry.ObservedAt == "" || !strings.HasSuffix(entry.ObservedAt, "Z") {
			t.Fatalf("%s: observed_at = %q, want a UTC timestamp", entry.File, entry.ObservedAt)
		}
		if !strings.Contains(entry.Reason, "V2-027") {
			t.Fatalf("%s: reason does not name the task that authored it: %q", entry.File, entry.Reason)
		}
		if strings.Contains(strings.ToLower(entry.Reason), "captured from") {
			t.Fatalf("%s: reason claims a capture: %q", entry.File, entry.Reason)
		}
	}
	for _, name := range provider.PoolNames() {
		if perProvider[name] != len(fixtureMatrix) {
			t.Fatalf("%s has %d manifest entries, want one per case in the matrix (%d)", name, perProvider[name], len(fixtureMatrix))
		}
	}
	// The manifest's own note must say the provenance in words, so a reader
	// of the directory alone cannot mistake these for captured responses.
	for _, phrase := range []string{"hand authored", "authored AGAINST", "no response was captured"} {
		if !strings.Contains(manifest.Note, phrase) {
			t.Fatalf("the manifest note does not state %q: %q", phrase, manifest.Note)
		}
	}
	t.Logf("%d entries, %d per provider, every one shape_source=projected-shape-hand-authored and corresponding_live_exercise=provider-live-multi; declared versions %v match the measured --version output", len(manifest.Entries), len(fixtureMatrix), measuredCLIVersions)
}

// TestM6CompletionConditionsAreNotClaimedAnywhereInThisPackage asserts the
// absence this task must not claim. The phrase the roadmap's own validation
// document uses for a real Preview exercise with a real Provider must not
// appear in this package at all: reusing it for a contract-fixture matrix
// would launder a fixture test into a live claim, which is the one failure
// mode the ban exists to prevent.
func TestM6CompletionConditionsAreNotClaimedAnywhereInThisPackage(t *testing.T) {
	var scanned int
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++
		lowered := strings.ToLower(string(raw))
		// The phrase is assembled from two pieces rather than written as one
		// literal, so this file's own scan does not find itself.
		if strings.Contains(lowered, "capability"+" "+"exercised") {
			t.Fatalf("%s contains the phrase this task may not use", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned == 0 {
		t.Fatal("scanned zero files")
	}
	t.Logf("scanned %d files in this package and its testdata; the phrase is absent. This task satisfies none of M6's four completion conditions: conditions 1 and 4 name a real Provider CLI and a real process on a real kernel and are wholly V2-028's under gate rules G3.1 and G3.3, and conditions 2 and 3 are split with V2-028 owning the demonstration each one asks for", scanned)
}
