// Package ci computes the deterministic selective-CI plan from a component manifest.
package ci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type Manifest struct {
	Version     int         `json:"version"`
	AllOnChange []string    `json:"all_on_change"`
	Components  []Component `json:"components"`
}
type Component struct {
	ID                   string   `json:"id"`
	Roots                []string `json:"roots"`
	PublicContracts      []string `json:"public_contracts"`
	ContractDependencies []string `json:"contract_dependencies"`
	Dependencies         []string `json:"dependencies"`
	// VerificationDependencies names the components whose sources participate
	// in this component's verification without being one of its non-test
	// imports (dp-v2-045 d7). It is deliberately NOT consulted by Affected()
	// and is exempt from the acyclicity rule, because the test-import graph
	// really does contain cycles (runner <-> reconciler). It IS part of the
	// evidence-key closure, so an edge here still moves this component's key.
	VerificationDependencies []string `json:"verification_dependencies"`
	Check                    Check    `json:"check"`
}
type Check struct {
	Runner string `json:"runner"`
	Target string `json:"target"`
}
type Plan struct {
	Changed      []string          `json:"changed"`
	Selected     []string          `json:"selected"`
	All          bool              `json:"all"`
	EvidenceKeys map[string]string `json:"evidence_keys,omitempty"`
}

func Load(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err = json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	return m, Validate(m)
}
func Validate(m Manifest) error {
	if m.Version != 1 || len(m.Components) == 0 {
		return errors.New("manifest: version must be 1 and components non-empty")
	}
	ids := map[string]bool{}
	for _, c := range m.Components {
		if c.ID == "" || ids[c.ID] {
			return fmt.Errorf("manifest: duplicate/empty component %q", c.ID)
		}
		ids[c.ID] = true
		if len(c.Roots) == 0 {
			return fmt.Errorf("component %s has no roots", c.ID)
		}
		if c.Check.Runner == "" || c.Check.Target == "" {
			return fmt.Errorf("component %s has incomplete check", c.ID)
		}
		for _, d := range c.Dependencies {
			if !ids[d] { /* checked below after all IDs */
			}
		}
	}
	for _, c := range m.Components {
		for _, d := range c.Dependencies {
			if !ids[d] {
				return fmt.Errorf("component %s depends on unknown %s", c.ID, d)
			}
		}
		// verification_dependencies must name known components, but is NOT
		// fed into the cycle walk below: dp-v2-045 d7 exempts it on purpose.
		for _, d := range c.VerificationDependencies {
			if !ids[d] {
				return fmt.Errorf("component %s verification-depends on unknown %s", c.ID, d)
			}
			if d == c.ID {
				return fmt.Errorf("component %s verification-depends on itself", c.ID)
			}
		}
		for _, d := range c.ContractDependencies {
			if d == "" {
				return fmt.Errorf("component %s has empty contract dependency", c.ID)
			}
		}
	}
	state := map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("component dependency cycle at %s", id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, c := range m.Components {
			if c.ID == id {
				for _, d := range c.Dependencies {
					if err := visit(d); err != nil {
						return err
					}
				}
			}
		}
		state[id] = 2
		return nil
	}
	for _, c := range m.Components {
		if err := visit(c.ID); err != nil {
			return err
		}
	}
	return nil
}

// ValidateCheckTargets verifies that every declared make target exists in the repository.
func ValidateCheckTargets(m Manifest, root string) error {
	b, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return err
	}
	text := string(b)
	for _, c := range m.Components {
		needle := c.Check.Target + ":"
		if !strings.Contains(text, needle) {
			return fmt.Errorf("component %s check target %q is missing", c.ID, c.Check.Target)
		}
	}
	return nil
}
func match(pattern, path string) bool {
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "./")
	path = filepath.ToSlash(path)
	if pattern == path {
		return true
	}
	if strings.HasSuffix(pattern, "/**") {
		return strings.HasPrefix(path, strings.TrimSuffix(pattern, "/**")+"/")
	}
	if strings.HasSuffix(pattern, "/") {
		return strings.HasPrefix(path, pattern)
	}
	ok, _ := filepath.Match(pattern, path)
	return ok
}
func owner(m Manifest, path string) []string {
	var out []string
	for _, c := range m.Components {
		for _, r := range c.Roots {
			if match(r, path) {
				out = append(out, c.ID)
				break
			}
		}
	}
	return out
}
func ValidateTracked(m Manifest, files []string) error {
	for _, f := range files {
		o := owner(m, f)
		if len(o) != 1 {
			return fmt.Errorf("tracked file %s has %d owners (%s)", f, len(o), strings.Join(o, ","))
		}
	}
	return nil
}
func Affected(m Manifest, changed []string) (Plan, error) {
	if err := Validate(m); err != nil {
		return Plan{}, err
	}
	p := Plan{Changed: append([]string(nil), changed...), EvidenceKeys: map[string]string{}}
	for _, f := range changed {
		for _, g := range m.AllOnChange {
			if match(g, f) {
				p.All = true
			}
		}
	}
	selected := map[string]bool{}
	if p.All {
		for _, c := range m.Components {
			selected[c.ID] = true
		}
	} else {
		for _, f := range changed {
			for _, c := range m.Components {
				for _, r := range c.Roots {
					if match(r, f) {
						selected[c.ID] = true
						break
					}
				}
				for _, r := range c.PublicContracts {
					if match(r, f) {
						selected[c.ID] = true
					}
				}
			}
		}
		changedContracts := []string{}
		for _, f := range changed {
			for _, c := range m.Components {
				for _, r := range c.PublicContracts {
					if match(r, f) {
						changedContracts = append(changedContracts, r)
					}
				}
			}
		}
		for _, c := range m.Components {
			for _, r := range c.ContractDependencies {
				for _, x := range changedContracts {
					if x == r || match(x, r) || match(r, x) {
						selected[c.ID] = true
					}
				}
			}
		}
		changed := true
		for changed {
			changed = false
			for _, c := range m.Components {
				for _, d := range c.Dependencies {
					if selected[d] && !selected[c.ID] {
						selected[c.ID] = true
						changed = true
					}
				}
			}
		}
	}
	for _, c := range m.Components {
		if selected[c.ID] {
			p.Selected = append(p.Selected, c.ID)
		}
	}
	sort.Strings(p.Selected)
	return p, nil
}

// ListTracked returns the repository's tracked paths, sorted. It is the only
// part of evidence-key computation that shells out, which is what lets the
// key-sensitivity controls (dp-v2-045 d9 PC4-PC6) inject a file list instead.
func ListTracked(root string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "ls-files").Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		files = append(files, line)
	}
	sort.Strings(files)
	return files, nil
}

// EvidenceKeyWithManifest computes the component's evidence key over the
// repository's tracked files.
func EvidenceKeyWithManifest(root string, m Manifest, c Component) (string, error) {
	files, err := ListTracked(root)
	if err != nil {
		return "", err
	}
	return EvidenceKeyFromFiles(root, files, m, c)
}

// EvidenceKeyFromFiles computes the evidence key of c over the given file
// list, per dp-v2-045 d2. Every dependency-derived input is the depended-on
// component's own file set, reached through the transitive key closure, so
// appending one byte to a dependency's non-test source moves this key. Every
// variable-length field is length-prefixed, which removes the ambiguity the
// pre-v2 framing had (path bytes immediately followed by content bytes). No
// map is iterated: the closure ids and the patterns both pass through
// sort.Strings, and the file list is re-sorted here.
func EvidenceKeyFromFiles(root string, files []string, m Manifest, c Component) (string, error) {
	closureIDs := KeyClosure(m, c.ID)
	patterns := KeyPatterns(m, c.ID)
	unconditional := make(map[string]bool, len(UnconditionalKeyPaths))
	for _, p := range UnconditionalKeyPaths {
		unconditional[p] = true
	}

	h := sha256.New()
	fmt.Fprintf(h, "%s\n", KeyVersion)
	fmt.Fprintf(h, "component:%d:%s\n", len(c.ID), c.ID)
	joined := strings.Join(closureIDs, ",")
	fmt.Fprintf(h, "closure:%d:%s\n", len(joined), joined)

	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	for _, p := range sorted {
		if p == "" {
			continue
		}
		include := unconditional[p]
		for _, pattern := range patterns {
			if match(pattern, p) {
				include = true
			}
		}
		if !include {
			continue
		}
		b, e := os.ReadFile(filepath.Join(root, p))
		if e != nil {
			if os.IsNotExist(e) {
				continue
			} // deleted files remain in git index during migration
			return "", e
		}
		fmt.Fprintf(h, "file:%d:%s:%d:", len(p), p, len(b))
		h.Write(b)
		h.Write([]byte("\n"))
	}
	fmt.Fprintf(h, "check:%d:%s:%d:%s\n", len(c.Check.Runner), c.Check.Runner, len(c.Check.Target), c.Check.Target)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func Candidate(m Manifest, root, evidenceDir string) error {
	ids := make([]string, 0, len(m.Components))
	for _, c := range m.Components {
		ids = append(ids, c.ID)
	}
	return CandidateComponents(m, root, evidenceDir, ids)
}

// CandidateComponents verifies fresh evidence only for the affected closure.
// CI may use this after independently attesting that the unchanged component
// keys belong to a successful parent workflow run.
func CandidateComponents(m Manifest, root, evidenceDir string, ids []string) error {
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	files, err := ListTracked(root)
	if err != nil {
		return err
	}
	for _, c := range m.Components {
		if !wanted[c.ID] {
			continue
		}
		k, err := EvidenceKeyFromFiles(root, files, m, c)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(filepath.Join(evidenceDir, c.ID+"-"+k+".json"))
		if err != nil {
			return fmt.Errorf("missing evidence for %s key %s", c.ID, k)
		}
		var v struct {
			Result      string `json:"result"`
			EvidenceKey string `json:"evidence_key"`
		}
		if json.Unmarshal(b, &v) != nil || v.Result != "passed" || v.EvidenceKey != k {
			return fmt.Errorf("invalid evidence for %s", c.ID)
		}
	}
	for id := range wanted {
		found := false
		for _, c := range m.Components {
			if c.ID == id {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown candidate component %s", id)
		}
	}
	return nil
}
