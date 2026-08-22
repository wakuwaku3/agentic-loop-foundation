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
	Check                Check    `json:"check"`
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
func EvidenceKey(root string, c Component) (string, error) {
	return evidenceKey(root, c, nil)
}
func EvidenceKeyWithManifest(root string, m Manifest, c Component) (string, error) {
	return evidenceKey(root, c, m.Components)
}
func evidenceKey(root string, c Component, all []Component) (string, error) {
	h := sha256.New()
	cmd := exec.Command("git", "-C", root, "ls-files")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	patterns := append([]string{}, c.Roots...)
	patterns = append(patterns, c.PublicContracts...)
	patterns = append(patterns, c.ContractDependencies...)
	for _, d := range c.Dependencies {
		for _, dep := range all {
			if dep.ID == d {
				patterns = append(patterns, dep.PublicContracts...)
			}
		}
	}
	files := strings.Split(strings.TrimSpace(string(out)), "\n")
	sort.Strings(files)
	for _, p := range files {
		if p == "" {
			continue
		}
		include := p == "ci/components.json" || p == "go.mod" || p == "go.sum" || p == "devbox.lock"
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
		h.Write([]byte(p))
		h.Write(b)
	}
	h.Write([]byte(c.Check.Runner))
	h.Write([]byte(c.Check.Target))
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
	for _, c := range m.Components {
		if !wanted[c.ID] {
			continue
		}
		k, err := evidenceKey(root, c, m.Components)
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
