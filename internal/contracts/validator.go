package contracts

import (
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
	return validate(s, v, "", resolver)
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
