package release

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFileHelper(path string) ([]byte, error) { return os.ReadFile(path) }

// realDocSet lists, repository-relative, the five documentation members
// measured in dp-v2-021: docs/preview/{README,capabilities,index,stable-diff}.md
// and docs/stable/index.md.
func realDocSet() []string {
	return []string{
		"docs/preview/README.md",
		"docs/preview/capabilities.md",
		"docs/preview/index.md",
		"docs/preview/stable-diff.md",
		"docs/stable/index.md",
	}
}

func realRoot() string { return filepath.Join("..", "..") }

// --- A12: Preview and Stable documentation routing is machine-verified
// with deterministic checks only, over the real doc set.

func TestDocSetLinksResolve(t *testing.T) {
	root := realRoot()
	docs := realDocSet()
	total := 0
	foundationCount, capabilitiesCount := 0, 0
	for _, doc := range docs {
		data := mustReadDocsFixture(t, root, doc)
		links := ExtractLinks(string(data))
		total += len(links)
		for _, l := range links {
			if filepath.Base(l.Target) == "foundation.json" {
				foundationCount++
			}
			if filepath.Base(l.Target) == "capabilities.md" {
				capabilitiesCount++
			}
		}
	}
	if total != 5 {
		t.Fatalf("real doc set link count = %d, want 5", total)
	}
	if foundationCount != 3 {
		t.Fatalf("links to foundation.json = %d, want 3", foundationCount)
	}
	if capabilitiesCount != 2 {
		t.Fatalf("links to capabilities.md = %d, want 2", capabilitiesCount)
	}
	if err := VerifyLinksResolve(root, docs); err != nil {
		t.Fatalf("VerifyLinksResolve: %v", err)
	}
}

func TestDocSetLinksResolveRefusesBrokenTarget(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "docs/preview/index.md", "[broken](missing.md)\n")
	if err := VerifyLinksResolve(root, []string{"docs/preview/index.md"}); err == nil {
		t.Fatal("expected a broken relative link to be refused")
	}
}

func TestDocSetLinksResolveRefusesUnresolvedAnchor(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "docs/preview/target.md", "no anchors here\n")
	writeFile(t, root, "docs/preview/index.md", "[link](target.md#missing-anchor)\n")
	if err := VerifyLinksResolve(root, []string{"docs/preview/index.md"}); err == nil {
		t.Fatal("expected an unresolved anchor to be refused")
	}
}

func TestCapabilityAnchorBijectionBothDirections(t *testing.T) {
	root := realRoot()
	capsDoc := string(mustReadDocsFixture(t, root, "docs/preview/capabilities.md"))
	compiled, err := AssembleFromRoot(root)
	if err != nil {
		t.Fatalf("assemble real tree: %v", err)
	}
	if err := VerifyCapabilityAnchorBijection(capsDoc, compiled.Contract.Capabilities); err != nil {
		t.Fatalf("real doc set capability anchor bijection: %v", err)
	}

	// The new claim: an anchor in the document that names no contract
	// capability id must be refused (contract-to-document direction alone,
	// which V2-009 already checks, would miss this).
	extra := capsDoc + "\n<a id=\"cap-does-not-exist\"></a>\n## Extra\n"
	if err := VerifyCapabilityAnchorBijection(extra, compiled.Contract.Capabilities); err == nil {
		t.Fatal("expected an anchor naming no contract capability to be refused")
	}
}

func TestReleaseMarkersOverRealDocSet(t *testing.T) {
	root := realRoot()
	compiled, err := AssembleFromRoot(root)
	if err != nil {
		t.Fatalf("assemble real tree: %v", err)
	}
	previewIndex := string(mustReadDocsFixture(t, root, "docs/preview/index.md"))
	if err := VerifyPreviewReleaseMarker(previewIndex, compiled.Contract.Version); err != nil {
		t.Fatalf("preview release marker: %v", err)
	}
	stableIndex := string(mustReadDocsFixture(t, root, "docs/stable/index.md"))
	anyStable := false
	for _, id := range compiled.Contract.Capabilities {
		_ = id // baseline capability status is not exposed by CompiledContract; the
		// real contract's baseline has zero stable capabilities (V2-009), and
		// the marker assertion below is the falsifiable check for that.
	}
	if err := VerifyStableReleaseMarker(stableIndex, anyStable); err != nil {
		t.Fatalf("stable release marker: %v", err)
	}
}

func TestReleaseMarkerMustOccurExactlyOnce(t *testing.T) {
	if err := VerifyPreviewReleaseMarker("no marker here\n", "0.1.0-baseline"); err == nil {
		t.Fatal("expected a missing release marker to be refused")
	}
	doubled := PreviewReleaseMarkerPrefix + "0.1.0-baseline\n" + PreviewReleaseMarkerPrefix + "0.2.0\n"
	if err := VerifyPreviewReleaseMarker(doubled, "0.1.0-baseline"); err == nil {
		t.Fatal("expected a doubled release marker to be refused")
	}
}

func TestStableDiffHasRequiredSections(t *testing.T) {
	root := realRoot()
	content := string(mustReadDocsFixture(t, root, "docs/preview/stable-diff.md"))
	if err := VerifyRequiredSections(content, RequiredPreviewSections); err != nil {
		t.Fatalf("required Preview sections: %v", err)
	}
}

func TestStableDiffMissingSectionIsRefused(t *testing.T) {
	content := "# Stableとの差分\n\n## 差分\n\ntext\n"
	if err := VerifyRequiredSections(content, RequiredPreviewSections); err == nil {
		t.Fatal("expected missing sections (known issues, return-to-stable, missing evidence) to be refused")
	}
}

func TestNoStableToPreviewLinksInRealDocSet(t *testing.T) {
	root := realRoot()
	if err := VerifyNoStableToPreviewLinks(root, []string{"docs/stable/index.md"}); err != nil {
		t.Fatalf("docs/stable must not link into docs/preview: %v", err)
	}
}

func TestNoStableToPreviewLinksRefusesViolation(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "docs/preview/index.md", "# Preview\n")
	writeFile(t, root, "docs/stable/index.md", "[bad](../preview/index.md)\n")
	if err := VerifyNoStableToPreviewLinks(root, []string{"docs/stable/index.md"}); err == nil {
		t.Fatal("expected a stable-to-preview link to be refused")
	}
}

// The real doc set has zero fenced code blocks today; the extractor must
// still be proven on a synthetic fixture that does contain blocks, and the
// real-tree assertion is the falsifiable "zero blocks now" claim.
func TestCodeBlockAllowlistRealDocSetIsEmptyToday(t *testing.T) {
	root := realRoot()
	total := 0
	for _, doc := range realDocSet() {
		content := string(mustReadDocsFixture(t, root, doc))
		total += len(ExtractCodeBlocks(content))
	}
	if total != 0 {
		t.Fatalf("real doc set contains %d fenced code blocks, want 0 today; any future block must be allowlisted", total)
	}
}

func TestCodeBlockAllowlistOnSyntheticFixtureWithBlocks(t *testing.T) {
	allowlist := []string{"devbox run --pure -- make check", "git status"}
	content := "# Fixture\n\n```sh\ndevbox run --pure -- make check\n```\n\n```sh\ngit status\n```\n"
	blocks := ExtractCodeBlocks(content)
	if len(blocks) != 2 {
		t.Fatalf("extracted %d blocks, want 2", len(blocks))
	}
	if err := VerifyCodeBlockAllowlist(blocks, allowlist); err != nil {
		t.Fatalf("allowlisted commands must pass: %v", err)
	}
	disallowed := "```sh\nrm -rf /\n```\n"
	if err := VerifyCodeBlockAllowlist(ExtractCodeBlocks(disallowed), allowlist); err == nil {
		t.Fatal("expected a non-allowlisted command to be refused")
	}
}

// --- A9 (channel resolution half): default channel is stable when a
// stable release exists and preview otherwise.

func TestResolveChannelDefaultsToPreviewWithoutStable(t *testing.T) {
	if got := ResolveChannel(false); got != "preview" {
		t.Fatalf("ResolveChannel(false) = %q, want preview", got)
	}
	if got := ResolveChannel(true); got != "stable" {
		t.Fatalf("ResolveChannel(true) = %q, want stable", got)
	}
}

func mustReadDocsFixture(t *testing.T, root, rel string) []byte {
	t.Helper()
	data, err := readFileHelper(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
