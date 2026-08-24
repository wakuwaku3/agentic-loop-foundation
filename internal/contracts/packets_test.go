package contracts

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestTaskPacketsValidate keeps every stored Design Packet and Work Order under
// .agents/v2/packets/ valid against its schema, so a durable checkpoint cannot
// drift away from the contract a resuming agent will read it with.
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
		default:
			t.Errorf("%s: packet name must end with -design-packet.json or -work-order.json", name)
			continue
		}
		if err := ValidateFile(filepath.Join(root, "contracts", "schemas", schema), mustRead(t, path)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
