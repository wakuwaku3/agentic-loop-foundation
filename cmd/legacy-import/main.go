package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/takushi/agentic-loop-foundation/v2/internal/legacyimport"
)

func main() {
	maxIssues := flag.Int("max-issues", 1000, "maximum exported issues")
	maxText := flag.Int("max-text-bytes", 1<<20, "maximum canonical text bytes per issue")
	flag.Parse()
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 16<<20))
	decoder.DisallowUnknownFields()
	var input legacyimport.Export
	if err := decoder.Decode(&input); err != nil {
		fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		fatal(fmt.Errorf("legacy export must contain exactly one JSON value"))
	}
	manifest, err := legacyimport.Build(input, *maxIssues, *maxText)
	if err != nil {
		fatal(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(true)
	if err := encoder.Encode(manifest); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "legacy-import:", err)
	os.Exit(1)
}
