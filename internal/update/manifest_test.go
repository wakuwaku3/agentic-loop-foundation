package update

import (
	"strings"
	"testing"
)

// TestManifestAcceptedSetAndTheThreeDigestJoin pins section 3.3's join and
// section 3.4's accepted set together, because they are the same change: the
// one breaking bump to v2 buys the join fields, and the accepted set is what
// makes it the last breaking bump. The join fields are values copied into
// the manifest; nothing here computes a bundle digest, and this package
// imports no part of internal/release to obtain one.
func TestManifestAcceptedSetAndTheThreeDigestJoin(t *testing.T) {
	f := newFixture(t)
	if len(AcceptedManifestSchemas) != 2 || AcceptedManifestSchemas[0] != ManifestSchemaV2 || ManifestSchema != ManifestSchemaV2 {
		t.Fatalf("accepted set = %v, current = %q", AcceptedManifestSchemas, ManifestSchema)
	}

	// The current id verifies and carries all three claims: the binary
	// digest that can be re-evaluated on disk, the signature that says who
	// authorised the pair, and the bundle digest that names the gated tree.
	manifest, err := Verify(f.bundle(t, "1.0.0", []byte("one"), nil), f.anchors, 1)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.BundleDigest != digestOf("bundle:1.0.0") || manifest.CandidateID != candidateOf("1.0.0") || manifest.ContractRelease == "" || manifest.ContractDigest == "" {
		t.Fatalf("v2 manifest lost a coordinate: %+v", manifest)
	}

	// An accepted older id still verifies, which is the property that makes
	// a future bump additive rather than a flag day.
	older := f.bundle(t, "1.0.0", []byte("one"), func(m *Manifest) {
		m.Schema = ManifestSchemaV1
		m.BundleDigest = ""
		m.CandidateID = ""
		m.ContractRelease = ""
		m.ContractDigest = ""
		m.RunnerAPIMin = 0
		m.RunnerAPIMax = 0
	})
	if _, err := Verify(older, f.anchors, 1); err != nil {
		t.Fatalf("accepted older manifest id refused: %v", err)
	}

	refusals := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"id-outside-the-accepted-set", func(m *Manifest) { m.Schema = "agentic-loop/module-manifest/v3" }},
		{"empty-id", func(m *Manifest) { m.Schema = "" }},
		{"v2-without-a-bundle-digest", func(m *Manifest) { m.BundleDigest = "" }},
		{"v2-with-a-malformed-bundle-digest", func(m *Manifest) { m.BundleDigest = "not-a-digest" }},
		{"v2-without-a-candidate", func(m *Manifest) { m.CandidateID = "" }},
		{"v2-without-a-contract-release", func(m *Manifest) { m.ContractRelease = "" }},
		{"v2-without-a-contract-digest", func(m *Manifest) { m.ContractDigest = "" }},
		{"v2-without-a-runner-api-range", func(m *Manifest) { m.RunnerAPIMin, m.RunnerAPIMax = 0, 0 }},
		{"v2-with-an-inverted-runner-api-range", func(m *Manifest) { m.RunnerAPIMin, m.RunnerAPIMax = 3, 2 }},
		{"older-id-smuggling-v2-coordinates", func(m *Manifest) { m.Schema = ManifestSchemaV1 }},
		{"a-bundle-that-installs-the-launcher", func(m *Manifest) { m.Roles = []string{"runner", BootstrapperRole} }},
		{"inverted-schema-interval", func(m *Manifest) { m.SchemaMin, m.SchemaMax = 4, 2 }},
		{"non-positive-schema-floor", func(m *Manifest) { m.SchemaMin = 0 }},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Verify(f.bundle(t, "1.0.0", []byte("one"), tc.mutate), f.anchors, 1); err == nil {
				t.Fatal("accepted")
			} else {
				t.Logf("verdict: %v", err)
			}
		})
	}

	// The two decoder-level refusals the measured tree already had, kept.
	signed := f.bundle(t, "1.0.0", []byte("one"), nil)
	unknownField := Bundle{Manifest: append(append([]byte(nil), strings.TrimSuffix(string(signed.Manifest), "}")...), []byte(`,"extra":1}`)...), Binary: signed.Binary, Signature: signed.Signature}
	if _, err := Verify(unknownField, f.anchors, 1); err == nil {
		t.Fatal("unknown manifest field accepted")
	}
	trailing := Bundle{Manifest: append(append([]byte(nil), signed.Manifest...), []byte(" {}")...), Binary: signed.Binary, Signature: signed.Signature}
	if _, err := Verify(trailing, f.anchors, 1); err == nil {
		t.Fatal("trailing JSON value accepted")
	}
}
