package contracts

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestTaskPacketsValidate keeps every stored Design Packet, Work Order and
// standing authorisation under .agents/v2/packets/ valid against its schema, so
// a durable checkpoint cannot drift away from the contract a resuming agent will
// read it with. Validating the standing authorisation here is a shape check
// only: it proves the record is well formed, never that a human wrote it.
func TestTaskPacketsValidate(t *testing.T) {
	root := filepath.Join("..", "..")
	paths, err := filepath.Glob(filepath.Join(root, ".agents", "v2", "packets", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		name := filepath.Base(path)
		var schema string
		switch {
		case strings.HasSuffix(name, "-design-packet.json"):
			schema = "design-packet.json"
		case strings.HasSuffix(name, "-work-order.json"):
			schema = "work-order.json"
		// A standing authorisation is not a task packet, but it lives here
		// because the preflight schema constrains subject_path to this
		// directory. It is validated against its own schema rather than
		// skipped, so a malformed authorisation cannot sit unchecked next to
		// the packets that reference it.
		case name == "provider-standing-authorization.json":
			schema = "provider-standing-authorization.json"
		default:
			t.Errorf("%s: packet name must end with -design-packet.json or -work-order.json, or be provider-standing-authorization.json", name)
			continue
		}
		if err := ValidateFile(filepath.Join(root, "contracts", "schemas", schema), mustRead(t, path)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
