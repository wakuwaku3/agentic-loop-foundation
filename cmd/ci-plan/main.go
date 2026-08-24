package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/takushi/agentic-loop-foundation/v2/internal/ci"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	defaultTaskID        = "V2-000"
	defaultCorrelationID = "local-component-evidence"
)

var taskIDPattern = regexp.MustCompile(`^V2-[0-9]{3}$`)

func main() {
	manifest := flag.String("manifest", "ci/components.json", "manifest path")
	changed := flag.String("changed", "", "comma/newline separated paths")
	all := flag.Bool("all", false, "select every manifest component regardless of --changed")
	keys := flag.Bool("keys", false, "populate evidence keys for the selected components")
	candidate := flag.Bool("candidate", false, "check evidence for every component")
	candidateChanged := flag.String("candidate-changed", "", "verify evidence for the affected closure of these paths")
	execute := flag.Bool("execute", false, "run selected component checks and write evidence")
	component := flag.String("component", "", "run one component")
	evidenceOut := flag.String("evidence-out", "build/evidence", "evidence output directory")
	evidence := flag.String("evidence-dir", "build/evidence", "evidence directory")
	root := flag.String("root", ".", "repository root")
	tracked := flag.String("tracked", "", "comma/newline separated tracked paths to validate")
	taskID := flag.String("task-id", "", "task id recorded in evidence (defaults to "+defaultTaskID+")")
	correlationID := flag.String("correlation-id", "", "correlation id recorded in evidence (defaults to "+defaultCorrelationID+")")
	flag.Parse()

	resolvedTaskID := resolveIdentity(*taskID, defaultTaskID)
	resolvedCorrelationID := resolveIdentity(*correlationID, defaultCorrelationID)
	if !taskIDPattern.MatchString(resolvedTaskID) {
		fail(fmt.Errorf("invalid --task-id %q: must match %s", resolvedTaskID, taskIDPattern.String()))
	}

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
	if *all {
		p.All = true
		ids := make([]string, 0, len(m.Components))
		for _, c := range m.Components {
			ids = append(ids, c.ID)
		}
		sort.Strings(ids)
		p.Selected = ids
	}
	if *keys {
		p.EvidenceKeys = map[string]string{}
		for _, id := range p.Selected {
			c, ok := lookupComponent(m, id)
			if !ok {
				fail(fmt.Errorf("unknown component %s", id))
			}
			key, err := ci.EvidenceKeyWithManifest(*root, m, c)
			if err != nil {
				fail(err)
			}
			p.EvidenceKeys[id] = key
		}
	}
	if *execute {
		if *component != "" {
			p.Selected = []string{*component}
		}
		for _, id := range p.Selected {
			c, ok := lookupComponent(m, id)
			if !ok {
				fail(fmt.Errorf("unknown component %s", id))
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
			record := evidenceRecord(id, key, c, resolvedTaskID, resolvedCorrelationID, time.Now().UTC())
			b, _ := json.Marshal(record)
			if err := os.WriteFile(filepath.Join(*evidenceOut, id+"-"+key+".json"), b, 0644); err != nil {
				fail(err)
			}
		}
	}
	output(p)
}

// resolveIdentity falls back to def when flag is empty or whitespace-only.
func resolveIdentity(flagValue, def string) string {
	if strings.TrimSpace(flagValue) == "" {
		return def
	}
	return flagValue
}

func lookupComponent(m ci.Manifest, id string) (ci.Component, bool) {
	for _, x := range m.Components {
		if x.ID == id {
			return x, true
		}
	}
	return ci.Component{}, false
}

// evidenceRecord builds the JSON-serializable evidence record for one
// component's executed check. The id is component-<id>-<key[:16]>.
func evidenceRecord(id, key string, c ci.Component, taskID, correlationID string, now time.Time) map[string]any {
	ts := now.Format(time.RFC3339)
	return map[string]any{
		"schema_version": "v1",
		"id":             "component-" + id + "-" + key[:16],
		"kind":           "evidence",
		"created_at":     ts,
		"observed_at":    ts,
		"correlation_id": correlationID,
		"task_id":        taskID,
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
