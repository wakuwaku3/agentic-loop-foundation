package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestSchemasAndFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "contracts")
	schemaDir := filepath.Join(root, "schemas")
	entries, err := os.ReadDir(schemaDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		schemaPath := filepath.Join(schemaDir, entry.Name())
		schema, err := os.ReadFile(schemaPath)
		if err != nil {
			t.Fatal(err)
		}
		var parsed any
		if err := json.Unmarshal(schema, &parsed); err != nil {
			t.Errorf("%s: %v", entry.Name(), err)
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		valid := filepath.Join(root, "fixtures", "valid", entry.Name())
		invalid := filepath.Join(root, "fixtures", "invalid", entry.Name())
		for _, tc := range []struct {
			path string
			want bool
		}{{valid, true}, {invalid, false}} {
			value, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("%s: %v", tc.path, err)
			}
			err = Validate(schema, value, ResolveSchemaRef(schemaDir))
			if (err == nil) != tc.want {
				t.Errorf("%s fixture %s: want valid=%v, got %v", name, tc.path, tc.want, err)
			}
		}
	}
}

func TestSchemasRejectSensitiveFieldNames(t *testing.T) {
	root := filepath.Join("..", "..", "contracts", "schemas")
	var paths []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && strings.HasSuffix(path, ".json") {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Strings(paths)
	for _, path := range paths {
		b, _ := os.ReadFile(path)
		text := strings.ToLower(string(b))
		for _, forbidden := range []string{"password", "credential", "raw_prompt", "raw_provider_output"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains forbidden field name %q", path, forbidden)
			}
		}
	}
}
