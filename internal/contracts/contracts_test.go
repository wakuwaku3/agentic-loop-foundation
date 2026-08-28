package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSchemasAndFixtures validates only contracts used by the running system.
// Historical task, evidence-ledger and handoff schemas were removed with the
// file-mediated coordination mechanism.
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
		for _, tc := range []struct {
			path string
			want bool
		}{
			{filepath.Join(root, "fixtures", "valid", entry.Name()), true},
			{filepath.Join(root, "fixtures", "invalid", entry.Name()), false},
		} {
			value, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("%s: %v", tc.path, err)
			}
			err = Validate(schema, value, ResolveSchemaRef(schemaDir))
			if (err == nil) != tc.want {
				t.Errorf("%s: want valid=%v, got %v", tc.path, tc.want, err)
			}
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(mustRead(t, path), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func ValidateFile(schemaPath string, value []byte) error {
	schema, err := os.ReadFile(schemaPath)
	if err != nil {
		return err
	}
	return Validate(schema, value, ResolveSchemaRef(filepath.Dir(schemaPath)))
}
