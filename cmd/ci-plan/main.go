package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/takushi/agentic-loop-foundation/v2/internal/ci"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	manifest := flag.String("manifest", "ci/components.json", "manifest path")
	changed := flag.String("changed", "", "comma/newline separated paths")
	candidate := flag.Bool("candidate", false, "check evidence for every component")
	candidateChanged := flag.String("candidate-changed", "", "verify evidence for the affected closure of these paths")
	execute := flag.Bool("execute", false, "run selected component checks and write evidence")
	component := flag.String("component", "", "run one component")
	evidenceOut := flag.String("evidence-out", "build/evidence", "evidence output directory")
	evidence := flag.String("evidence-dir", "build/evidence", "evidence directory")
	root := flag.String("root", ".", "repository root")
	tracked := flag.String("tracked", "", "comma/newline separated tracked paths to validate")
	flag.Parse()
	m, err := ci.Load(*manifest)
	if err != nil {
		fail(err)
	}
	if err := ci.ValidateCheckTargets(m, *root); err != nil {
		fail(err)
	}
	if *tracked != "" {
		if err := ci.ValidateTracked(m, split(*tracked)); err != nil {
			fail(err)
		}
	}
	if *candidate {
		var candidateErr error
		if *candidateChanged != "" {
			plan, err := ci.Affected(m, split(*candidateChanged))
			if err != nil {
				fail(err)
			}
			candidateErr = ci.CandidateComponents(m, *root, *evidence, plan.Selected)
		} else {
			candidateErr = ci.Candidate(m, *root, *evidence)
		}
		if candidateErr != nil {
			fail(candidateErr)
		}
		output(map[string]any{"candidate": true})
		return
	}
	p, err := ci.Affected(m, split(*changed))
	if err != nil {
		fail(err)
	}
	if *execute {
		if *component != "" {
			p.Selected = []string{*component}
		}
		for _, id := range p.Selected {
			var c ci.Component
			for _, x := range m.Components {
				if x.ID == id {
					c = x
				}
			}
			cmd := exec.Command(c.Check.Runner, c.Check.Target)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				fail(err)
			}
			key, err := ci.EvidenceKeyWithManifest(*root, m, c)
			if err != nil {
				fail(err)
			}
			if err := os.MkdirAll(*evidenceOut, 0755); err != nil {
				fail(err)
			}
			now := time.Now().UTC().Format(time.RFC3339)
			record := map[string]any{
				"schema_version": "v1",
				"id":             "component-" + id + "-" + key[:16],
				"kind":           "evidence",
				"created_at":     now,
				"observed_at":    now,
				"correlation_id": "m0-candidate",
				"task_id":        "V2-005",
				"component":      id,
				"evidence_key":   key,
				"result":         "passed",
				"checks": []map[string]any{{
					"name":              c.Check.Target,
					"status":            "passed",
					"argv":              []string{c.Check.Runner, c.Check.Target},
					"working_directory": ".",
					"timeout_seconds":   600,
				}},
			}
			b, _ := json.Marshal(record)
			if err := os.WriteFile(filepath.Join(*evidenceOut, id+"-"+key+".json"), b, 0644); err != nil {
				fail(err)
			}
		}
	}
	output(p)
}
func split(s string) []string {
	var out []string
	for _, x := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t' }) {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}
func output(v any)   { b, _ := json.Marshal(v); fmt.Println(string(b)) }
func fail(err error) { fmt.Fprintln(os.Stderr, "ci-plan:", err); os.Exit(1) }
