// Deterministic Preview/Stable documentation routing checks (dp-v2-021 d10).
//
// Every check here is link resolution, anchor resolution, or fixed-format
// line parsing over document bytes; none of it is AI document review, and
// none of it accepts a digest value as an expected input (dp-v2-021 d14: a
// documentation member must never contain its own digest).
package release

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// PreviewReleaseMarkerPrefix and StableReleaseMarkerPrefix are the fixed,
// documented line formats a channel index must carry exactly once. The line
// is "<prefix><value>" with no other content on the line. A free-prose
// sentence is not a marker; only this exact line format is machine-checked.
const (
	PreviewReleaseMarkerPrefix = "Release: "
	StableReleaseMarkerPrefix  = "Stable release: "
	// StableReleaseMarkerNone is the fixed value the stable marker must carry
	// exactly while no capability has status stable.
	StableReleaseMarkerNone = "none"
)

// RequiredPreviewSections names the four sections validation.md section 4 /
// release-contract.md section 5 require the Preview channel's diff document
// to carry. The match is against a "## " heading line containing the text.
var RequiredPreviewSections = []string{
	"差分",
	"既知の問題",
	"Stableへ戻す方法",
	"昇格に不足している実証",
}

// Link is one relative markdown link found in a document: [text](target) or
// [text](target#anchor). Absolute (http(s):, mailto:) links are not
// collected; this package only verifies links this repository can resolve
// on disk.
type Link struct {
	Target string
	Anchor string
}

var linkPattern = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)
var anchorTagPattern = regexp.MustCompile(`<a\s+id="([^"]+)"\s*>\s*</a>`)
var fencedBlockPattern = regexp.MustCompile("(?s)```([a-zA-Z0-9_-]*)\\n(.*?)```")

func isRelativeLink(target string) bool {
	if target == "" {
		return false
	}
	for _, prefix := range []string{"http://", "https://", "mailto:", "#"} {
		if strings.HasPrefix(target, prefix) {
			return false
		}
	}
	return true
}

// ExtractLinks returns every relative markdown link in content, in order.
func ExtractLinks(content string) []Link {
	var links []Link
	for _, m := range linkPattern.FindAllStringSubmatch(content, -1) {
		raw := m[1]
		if !isRelativeLink(raw) {
			continue
		}
		target, anchor, _ := strings.Cut(raw, "#")
		links = append(links, Link{Target: target, Anchor: anchor})
	}
	return links
}

// ExtractAnchors returns every <a id="..."></a> anchor id declared in
// content, in document order.
func ExtractAnchors(content string) []string {
	var out []string
	for _, m := range anchorTagPattern.FindAllStringSubmatch(content, -1) {
		out = append(out, m[1])
	}
	return out
}

// VerifyLinksResolve checks that, for every relative link in every listed
// repository-relative document, the target file exists and, if the link
// names an anchor, that anchor occurs in the target file's
// <a id="..."></a> tags exactly once.
func VerifyLinksResolve(root string, docs []string) error {
	for _, doc := range docs {
		docAbs := filepath.Join(root, filepath.FromSlash(doc))
		content, err := os.ReadFile(docAbs)
		if err != nil {
			return fmt.Errorf("read %s: %w", doc, err)
		}
		docDir := filepath.Dir(docAbs)
		for _, link := range ExtractLinks(string(content)) {
			targetAbs := filepath.Join(docDir, filepath.FromSlash(link.Target))
			targetData, err := os.ReadFile(targetAbs)
			if err != nil {
				return fmt.Errorf("%s: link target %q does not resolve: %w", doc, link.Target, err)
			}
			if link.Anchor == "" {
				continue
			}
			count := 0
			for _, id := range ExtractAnchors(string(targetData)) {
				if id == link.Anchor {
					count++
				}
			}
			if count != 1 {
				return fmt.Errorf("%s: link target %q#%s resolves to %d anchors, want exactly 1", doc, link.Target, link.Anchor, count)
			}
		}
	}
	return nil
}

// VerifyCapabilityAnchorBijection asserts that the anchor set declared in
// capabilitiesDoc is exactly the contract's capability id set, in both
// directions: every contract id has an anchor (V2-009's existing claim) and
// every anchor in the document names a contract capability id (the new
// claim this task adds).
func VerifyCapabilityAnchorBijection(capabilitiesDoc string, contractIDs []string) error {
	anchors := ExtractAnchors(capabilitiesDoc)
	anchorSet := map[string]int{}
	for _, a := range anchors {
		anchorSet[a]++
	}
	idSet := map[string]bool{}
	for _, id := range contractIDs {
		idSet[id] = true
		if anchorSet[id] != 1 {
			return fmt.Errorf("contract capability %q has %d anchors in the document, want exactly 1", id, anchorSet[id])
		}
	}
	for anchor := range anchorSet {
		if !idSet[anchor] {
			return fmt.Errorf("document anchor %q names no contract capability id", anchor)
		}
	}
	return nil
}

// ParseMarker returns the value of a fixed-format "<prefix>value" line and
// how many times that prefix occurs as a standalone line. A marker occurring
// zero or more than once is a caller error, not a silent pick-first.
func ParseMarker(content, prefix string) (value string, occurrences int) {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if strings.HasPrefix(trimmed, prefix) {
			occurrences++
			value = strings.TrimPrefix(trimmed, prefix)
		}
	}
	return value, occurrences
}

// VerifyPreviewReleaseMarker asserts the preview channel index declares the
// fixed-format release marker exactly once and it equals the contract's
// release string.
func VerifyPreviewReleaseMarker(previewIndexContent, contractRelease string) error {
	value, occurrences := ParseMarker(previewIndexContent, PreviewReleaseMarkerPrefix)
	if occurrences != 1 {
		return fmt.Errorf("preview index declares the release marker %d times, want exactly 1", occurrences)
	}
	if value != contractRelease {
		return fmt.Errorf("preview release marker is %q, want contract release %q", value, contractRelease)
	}
	return nil
}

// VerifyStableReleaseMarker asserts the stable channel index declares the
// fixed-format stable marker exactly once, and that it is exactly "none"
// while no capability has status stable.
func VerifyStableReleaseMarker(stableIndexContent string, anyCapabilityStable bool) error {
	value, occurrences := ParseMarker(stableIndexContent, StableReleaseMarkerPrefix)
	if occurrences != 1 {
		return fmt.Errorf("stable index declares the stable marker %d times, want exactly 1", occurrences)
	}
	if !anyCapabilityStable && value != StableReleaseMarkerNone {
		return fmt.Errorf("stable marker is %q, want %q while no capability has status stable", value, StableReleaseMarkerNone)
	}
	if anyCapabilityStable && value == StableReleaseMarkerNone {
		return fmt.Errorf("stable marker is %q but at least one capability has status stable", StableReleaseMarkerNone)
	}
	return nil
}

// VerifyRequiredSections asserts every heading in required occurs as a
// "## <text containing heading>" line in content.
func VerifyRequiredSections(content string, required []string) error {
	var headings []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			headings = append(headings, strings.TrimPrefix(trimmed, "## "))
		}
	}
	for _, want := range required {
		found := false
		for _, h := range headings {
			if strings.Contains(h, want) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("required section %q is missing", want)
		}
	}
	return nil
}

// VerifyNoStableToPreviewLinks asserts that no document under the stable
// docs set links into the preview docs set.
func VerifyNoStableToPreviewLinks(root string, stableDocs []string) error {
	for _, doc := range stableDocs {
		docAbs := filepath.Join(root, filepath.FromSlash(doc))
		content, err := os.ReadFile(docAbs)
		if err != nil {
			return fmt.Errorf("read %s: %w", doc, err)
		}
		docDir := filepath.Dir(docAbs)
		for _, link := range ExtractLinks(string(content)) {
			targetAbs := filepath.Join(docDir, filepath.FromSlash(link.Target))
			targetRel, err := filepath.Rel(root, targetAbs)
			if err != nil {
				return err
			}
			targetRel = filepath.ToSlash(targetRel)
			if strings.HasPrefix(targetRel, "docs/preview/") {
				return fmt.Errorf("%s links into docs/preview/: %s", doc, targetRel)
			}
		}
	}
	return nil
}

// CodeBlock is one fenced code block extracted from a markdown document.
type CodeBlock struct {
	Info string
	Body string
}

// ExtractCodeBlocks returns every fenced (```) code block in content.
func ExtractCodeBlocks(content string) []CodeBlock {
	var out []CodeBlock
	for _, m := range fencedBlockPattern.FindAllStringSubmatch(content, -1) {
		out = append(out, CodeBlock{Info: m[1], Body: m[2]})
	}
	return out
}

// VerifyCodeBlockAllowlist refuses any fenced code block whose first
// non-blank line does not start with one of allowlist's entries.
func VerifyCodeBlockAllowlist(blocks []CodeBlock, allowlist []string) error {
	for _, block := range blocks {
		firstLine := ""
		for _, line := range strings.Split(block.Body, "\n") {
			if strings.TrimSpace(line) != "" {
				firstLine = strings.TrimSpace(line)
				break
			}
		}
		if firstLine == "" {
			return fmt.Errorf("empty fenced code block is not allowlistable")
		}
		allowed := false
		for _, prefix := range allowlist {
			if strings.HasPrefix(firstLine, prefix) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("fenced code block command %q is not on the allowlist", firstLine)
		}
	}
	return nil
}

// ResolveChannel returns the default documentation channel: "stable" when a
// stable release exists, "preview" otherwise (dp-v2-021 A9).
func ResolveChannel(stableExists bool) string {
	if stableExists {
		return "stable"
	}
	return "preview"
}

// SortedCopy returns a sorted copy of ids, used by tests that need a
// deterministic comparison independent of map iteration order.
func SortedCopy(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}
