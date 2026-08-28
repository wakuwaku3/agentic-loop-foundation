package contracts

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Validate performs the deliberately small JSON Schema subset needed by the
// contracts. It is dependency-free so the contract gate works in the bootstrap
// environment; a full validator can be introduced without changing schemas.
func Validate(schema, value []byte, resolver func(string) ([]byte, error)) error {
	var s, v any
	if err := json.Unmarshal(schema, &s); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	if err := json.Unmarshal(value, &v); err != nil {
		return fmt.Errorf("value: %w", err)
	}
	if err := validate(s, v, "", resolver); err != nil {
		return err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	_ = obj
	return nil
}

func validateTaskState(state map[string]any) error {
	taskID := stringValue(state["task_id"])
	if state["id"] != "ts-"+strings.ToLower(taskID) {
		return fmt.Errorf("/id: does not match task_id")
	}
	for _, field := range []string{"dependencies", "repair_of"} {
		if raw, present := state[field]; present {
			if err := uniqueTaskIDs(raw, taskID, "/"+field); err != nil {
				return err
			}
		}
	}
	if state["status"] == "blocked" && stringValue(state["block_reason"]) == "" {
		return fmt.Errorf("/block_reason: required for blocked state")
	}
	if err := checkBudget(state["retry_budget"], "/retry_budget"); err != nil {
		return err
	}
	transitions, ok := state["transitions"].([]any)
	if !ok || len(transitions) == 0 {
		return fmt.Errorf("/transitions: empty")
	}
	var previous string
	var previousBudget map[string]any
	for i, raw := range transitions {
		transition, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("/transitions/%d: invalid", i)
		}
		sequence, ok := transition["sequence"].(float64)
		if !ok || int(sequence) != i+1 {
			return fmt.Errorf("/transitions/%d/sequence: must be contiguous", i)
		}
		from, to := transition["from_status"], stringValue(transition["to_status"])
		if i == 0 {
			if from != nil {
				return fmt.Errorf("/transitions/0/from_status: must be null")
			}
		} else {
			if stringValue(from) != previous {
				return fmt.Errorf("/transitions/%d/from_status: disconnected", i)
			}
			if !allowedTaskTransition(previous, to) {
				return fmt.Errorf("/transitions/%d: illegal %s -> %s", i, previous, to)
			}
		}
		for _, refs := range []struct{ field, hash string }{{"input_refs", "input_hash"}, {"output_refs", "output_hash"}} {
			digest, err := refsDigest(transition[refs.field])
			if err != nil {
				return fmt.Errorf("/transitions/%d/%s: %w", i, refs.field, err)
			}
			if digest != stringValue(transition[refs.hash]) {
				return fmt.Errorf("/transitions/%d/%s: does not match refs", i, refs.hash)
			}
		}
		budget, ok := transition["retry_budget"].(map[string]any)
		if !ok {
			return fmt.Errorf("/transitions/%d/retry_budget: invalid", i)
		}
		if err := checkBudget(budget, fmt.Sprintf("/transitions/%d/retry_budget", i)); err != nil {
			return err
		}
		if previousBudget != nil && (number(budget["max_attempts"]) != number(previousBudget["max_attempts"]) || number(budget["attempts_used"]) < number(previousBudget["attempts_used"])) {
			return fmt.Errorf("/transitions/%d/retry_budget: cannot restore attempts", i)
		}
		previous, previousBudget = to, budget
	}
	last := transitions[len(transitions)-1].(map[string]any)
	for _, field := range []string{"status", "actor", "input_refs", "input_hash", "output_refs", "output_hash", "owner", "next_owner", "retry_budget"} {
		lastField := field
		if field == "status" {
			lastField = "to_status"
		}
		if !equal(state[field], last[lastField]) {
			return fmt.Errorf("/%s: must project the last transition", field)
		}
	}
	status := stringValue(state["status"])
	if (status == "complete" || status == "failed") && state["next_owner"] != nil {
		return fmt.Errorf("/next_owner: terminal state")
	}
	if status == "failed" && number(state["retry_budget"].(map[string]any)["remaining"]) != 0 {
		return fmt.Errorf("/retry_budget: failed state has remaining attempts")
	}
	return nil
}

func uniqueTaskIDs(raw any, self, path string) error {
	values, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("%s: invalid", path)
	}
	seen := map[string]bool{}
	for _, rawID := range values {
		id := stringValue(rawID)
		if id == "" || id == self || seen[id] {
			return fmt.Errorf("%s: empty, self, or duplicate reference", path)
		}
		seen[id] = true
	}
	return nil
}

func refsDigest(raw any) (string, error) {
	values, ok := raw.([]any)
	if !ok {
		return "", fmt.Errorf("invalid")
	}
	refs := make([]string, len(values))
	seen := map[string]bool{}
	for i, rawRef := range values {
		ref, ok := rawRef.(string)
		if !ok || ref == "" || seen[ref] {
			return "", fmt.Errorf("empty or duplicate reference")
		}
		seen[ref] = true
		refs[i] = ref
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

func checkBudget(raw any, path string) error {
	budget, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: invalid", path)
	}
	max, used, remaining := number(budget["max_attempts"]), number(budget["attempts_used"]), number(budget["remaining"])
	if max < 1 || used < 0 || used > max || remaining != max-used {
		return fmt.Errorf("%s: inconsistent", path)
	}
	return nil
}

func allowedTaskTransition(from, to string) bool {
	switch from {
	case "queued":
		return to == "running" || to == "blocked" || to == "failed"
	case "running":
		return to == "complete" || to == "blocked" || to == "failed"
	case "blocked":
		return to == "queued" || to == "failed"
	}
	return false
}

func validateEvidenceIndex(index map[string]any) error {
	entries, ok := index["entries"].([]any)
	if !ok {
		return fmt.Errorf("/entries: invalid")
	}
	seen := map[string]bool{}
	for i, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok || stringValue(entry["id"]) == "" || seen[stringValue(entry["id"])] {
			return fmt.Errorf("/entries/%d: duplicate or invalid id", i)
		}
		seen[stringValue(entry["id"])] = true
	}
	return nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func number(value any) float64 {
	n, _ := value.(float64)
	return n
}

func validate(s, v any, path string, resolver func(string) ([]byte, error)) error {
	m, ok := s.(map[string]any)
	if !ok {
		return nil
	}
	if ref, ok := m["$ref"].(string); ok {
		b, err := resolver(ref)
		if err != nil {
			return err
		}
		var rs any
		if err := json.Unmarshal(b, &rs); err != nil {
			return err
		}
		return validate(rs, v, path, resolver)
	}
	if c, ok := m["const"]; ok && !equal(c, v) {
		return fmt.Errorf("%s: value differs from const", path)
	}
	if es, ok := m["enum"].([]any); ok {
		found := false
		for _, e := range es {
			if equal(e, v) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s: value not in enum", path)
		}
	}
	if typ, ok := m["type"].(string); ok && !typeOK(typ, v) {
		return fmt.Errorf("%s: want %s", path, typ)
	}
	if types, ok := m["type"].([]any); ok {
		matched := false
		for _, rawType := range types {
			if typ, ok := rawType.(string); ok && typeOK(typ, v) {
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("%s: want one of declared types", path)
		}
	}
	if req, ok := m["required"].([]any); ok {
		obj, isObj := v.(map[string]any)
		if isObj {
			for _, r := range req {
				n := r.(string)
				if _, exists := obj[n]; !exists {
					return fmt.Errorf("%s: missing required %q", path, n)
				}
			}
		}
	}
	if obj, ok := v.(map[string]any); ok {
		props, _ := m["properties"].(map[string]any)
		if ap, exists := m["additionalProperties"]; exists {
			if b, isBool := ap.(bool); isBool && !b {
				for k := range obj {
					if _, declared := props[k]; !declared {
						return fmt.Errorf("%s: unknown property %q", path, k)
					}
				}
			}
		}
		for k, val := range obj {
			if ps, exists := props[k]; exists {
				if err := validate(ps, val, path+"/"+k, resolver); err != nil {
					return err
				}
			}
		}
	}
	if arr, ok := v.([]any); ok {
		if n, exists := m["minItems"]; exists && float64(len(arr)) < n.(float64) {
			return fmt.Errorf("%s: too few items", path)
		}
		if is, exists := m["items"]; exists {
			for i, val := range arr {
				if err := validate(is, val, fmt.Sprintf("%s/%d", path, i), resolver); err != nil {
					return err
				}
			}
		}
	}
	if str, ok := v.(string); ok {
		if n, exists := m["minLength"]; exists && len([]rune(str)) < int(n.(float64)) {
			return fmt.Errorf("%s: too short", path)
		}
		if n, exists := m["maxLength"]; exists && len([]rune(str)) > int(n.(float64)) {
			return fmt.Errorf("%s: too long", path)
		}
		if p, exists := m["pattern"]; exists {
			if !regexp.MustCompile(p.(string)).MatchString(str) {
				return fmt.Errorf("%s: pattern mismatch", path)
			}
		}
		if f, exists := m["format"]; exists && !validFormat(f.(string), str) {
			return fmt.Errorf("%s: invalid %s", path, f)
		}
	}
	if num, ok := v.(float64); ok {
		if n, exists := m["minimum"]; exists && num < n.(float64) {
			return fmt.Errorf("%s: below minimum", path)
		}
		if n, exists := m["maximum"]; exists && num > n.(float64) {
			return fmt.Errorf("%s: above maximum", path)
		}
	}
	return nil
}

func typeOK(t string, v any) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "integer":
		n, ok := v.(float64)
		return ok && n == float64(int64(n))
	case "number":
		_, ok := v.(float64)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	}
	return true
}
func equal(a, b any) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}
func validFormat(f, s string) bool {
	switch f {
	case "date-time":
		_, err := time.Parse(time.RFC3339, s)
		return err == nil
	case "uri":
		u, err := url.Parse(s)
		return err == nil && u.Scheme != ""
	}
	return true
}

// ResolveSchemaRef resolves the local references used by work-packet and evidence.
func ResolveSchemaRef(base string) func(string) ([]byte, error) {
	return func(ref string) ([]byte, error) {
		if strings.HasPrefix(ref, "http") {
			return nil, fmt.Errorf("remote ref not allowed: %s", ref)
		}
		return readFile(base + "/" + ref)
	}
}
