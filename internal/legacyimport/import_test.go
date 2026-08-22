package legacyimport

import (
	"strings"
	"testing"
)

func TestBuildIsDeterministicAndQuarantinesSecrets(t *testing.T) {
	export := Export{Schema: Schema, Issues: []Issue{
		{Number: 2, Title: "done", Labels: []string{"agent:merged"}},
		{Number: 1, Title: "problem", Body: "users cannot stop workers", Comments: []Comment{{Author: "owner", Body: "still happens"}}},
		{Number: 3, Title: "credential", Body: "token=abcdefghijklmnop"},
	}}
	manifest, err := Build(export, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 3 || manifest.Entries[0].SourceNumber != 1 || manifest.Entries[0].Disposition != Import {
		t.Fatalf("manifest=%#v", manifest)
	}
	if manifest.Entries[1].Disposition != Excluded || manifest.Entries[2].Disposition != Quarantine {
		t.Fatalf("manifest=%#v", manifest)
	}
	if manifest.Entries[2].Title != "" || strings.Contains(manifest.Entries[2].ProblemSource, "abcdefghijklmnop") {
		t.Fatal("quarantine leaked content")
	}
}

func TestBuildRejectsUnboundedOrDuplicateInput(t *testing.T) {
	if _, err := Build(Export{Schema: Schema, Issues: []Issue{{Number: 1}, {Number: 1}}}, 10, 10); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := Build(Export{Schema: Schema, Issues: []Issue{{Number: 1}}}, 0, 10); err == nil {
		t.Fatal("unbounded limit accepted")
	}
}
