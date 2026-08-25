package ci

import (
	"encoding/json"
	"fmt"
	"sort"
)

// KeyVersion is the version prefix written first into every evidence key
// (dp-v2-045 d2). Changing the framing requires changing this string, so an
// old key can never silently compare equal to a new one.
const KeyVersion = "agentic-loop/evidence-key/v2"

// KeyClosureVersion is the schema version of ci/key-closure.json.
const KeyClosureVersion = 1

// UnconditionalKeyPaths is the literal, sorted set of paths hashed into every
// component's evidence key regardless of that component's closure
// (dp-v2-045 d4). Makefile is here because every component's verification
// entrypoint is a make recipe; devbox.json because it declares the packages
// and scripts devbox run --pure executes; ci/key-closure.json because d10
// makes it the published form of the closure.
var UnconditionalKeyPaths = []string{
	"Makefile",
	"ci/components.json",
	"ci/key-closure.json",
	"devbox.json",
	"devbox.lock",
	"go.mod",
	"go.sum",
}

// KeyClosure returns the transitive closure of id over the union of
// dependencies and verification_dependencies, including id itself, sorted.
// The two edge kinds are indistinguishable once the walk starts (d3): the
// coarse rule is a strict superset of every refinement, so its sufficiency
// needs no per-check-target argument.
func KeyClosure(m Manifest, id string) []string {
	byID := make(map[string]Component, len(m.Components))
	for _, c := range m.Components {
		byID[c.ID] = c
	}
	visited := map[string]bool{id: true}
	queue := []string{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		c, ok := byID[cur]
		if !ok {
			continue
		}
		for _, next := range append(append([]string(nil), c.Dependencies...), c.VerificationDependencies...) {
			if !visited[next] {
				visited[next] = true
				queue = append(queue, next)
			}
		}
	}
	out := make([]string, 0, len(visited))
	for k := range visited {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// KeyPatterns returns the deduplicated, sorted path patterns whose matching
// tracked files are hashed into id's evidence key: for every member of the
// key closure, its roots, public_contracts and contract_dependencies.
func KeyPatterns(m Manifest, id string) []string {
	byID := make(map[string]Component, len(m.Components))
	for _, c := range m.Components {
		byID[c.ID] = c
	}
	seen := map[string]bool{}
	var out []string
	for _, member := range KeyClosure(m, id) {
		c, ok := byID[member]
		if !ok {
			continue
		}
		for _, p := range append(append(append([]string(nil), c.Roots...), c.PublicContracts...), c.ContractDependencies...) {
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// KeyClosureEntry is one component's published closure.
type KeyClosureEntry struct {
	Component string   `json:"component"`
	Closure   []string `json:"closure"`
	Patterns  []string `json:"patterns"`
}

// KeyClosureDocument is the published form of the evidence-key closure
// (ci/key-closure.json, dp-v2-045 d10). It carries no evidence key and no
// hash of any kind: the file is itself hashed into every key, so a key
// inside it would be an unsatisfiable fixed point.
type KeyClosureDocument struct {
	Version       int               `json:"version"`
	KeyVersion    string            `json:"key_version"`
	Unconditional []string          `json:"unconditional"`
	Components    []KeyClosureEntry `json:"components"`
}

// BuildKeyClosureDocument derives the published closure from the manifest.
func BuildKeyClosureDocument(m Manifest) KeyClosureDocument {
	unconditional := append([]string(nil), UnconditionalKeyPaths...)
	sort.Strings(unconditional)
	ids := make([]string, 0, len(m.Components))
	for _, c := range m.Components {
		ids = append(ids, c.ID)
	}
	sort.Strings(ids)
	entries := make([]KeyClosureEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, KeyClosureEntry{
			Component: id,
			Closure:   KeyClosure(m, id),
			Patterns:  KeyPatterns(m, id),
		})
	}
	return KeyClosureDocument{
		Version:       KeyClosureVersion,
		KeyVersion:    KeyVersion,
		Unconditional: unconditional,
		Components:    entries,
	}
}

// RenderKeyClosureDocument serializes the document to the exact bytes the
// tracked golden file must contain: two-space indented JSON plus one
// trailing newline.
func RenderKeyClosureDocument(d KeyClosureDocument) ([]byte, error) {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render key closure: %w", err)
	}
	return append(b, '\n'), nil
}
