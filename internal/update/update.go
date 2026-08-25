// Package update implements the small, provider-independent bootstrapper
// boundary. Versions are installed side-by-side and routing changes by one
// atomic symlink rename; an existing Stable version is never overwritten.
//
// This package is the local closure of M8 (docs/operations/self-update.md).
// It links only the standard library, and the Runner links none of it: the
// process that updates X is structurally not X (section 4.1), enforced by
// the go/ast guards in internal/update/source_guard_test.go and
// cmd/bootstrap/source_guard_test.go.
package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// The module manifest ids. ManifestSchemaV2 carries the section 3.3 join
// fields and the section 5.1 coordinates; ManifestSchemaV1 is the id the
// pre-M8 tree declared and is kept in the accepted set so the one breaking
// bump is also the last one (section 3.4).
const (
	ManifestSchemaV1 = "agentic-loop/module-manifest/v1"
	ManifestSchemaV2 = "agentic-loop/module-manifest/v2"

	// ManifestSchema is the id newly assembled bundles declare.
	ManifestSchema = ManifestSchemaV2
)

// AcceptedManifestSchemas is the ordered accepted set, newest first. It
// replaces the single-constant equality check so that later manifest
// evolution is itself expand / coexist / migrate / contract: a new id is
// added here in a Bootstrapper release, bundles start declaring it only
// after every machine accepts it, and an old id leaves the set only after
// no installed version still declares it.
var AcceptedManifestSchemas = []string{ManifestSchemaV2, ManifestSchemaV1}

// ManifestSchemaAccepted reports whether id is in the accepted set.
func ManifestSchemaAccepted(id string) bool {
	for _, accepted := range AcceptedManifestSchemas {
		if accepted == id {
			return true
		}
	}
	return false
}

// BootstrapperRole is the bundle role that must never exist: the component
// that verifies releases is not replaced by a release (section 4.3). A
// bundle declaring it is refused.
const BootstrapperRole = "bootstrapper"

var (
	versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[a-z0-9.-]+)?$`)
	digestPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Manifest is the signed version record: one record per version carrying
// the five coordinate groups of section 5.1 -- binary, canonical schema
// interval, contract, bundle and provenance.
type Manifest struct {
	Schema       string `json:"schema"`
	Version      string `json:"version"`
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	BinarySHA256 string `json:"binary_sha256"`
	SchemaMin    int    `json:"schema_min"`
	SchemaMax    int    `json:"schema_max"`

	// Provenance (section 3.1): which accepted anchor entry verifies this
	// manifest. Required by every accepted schema id, because selecting an
	// entry out of a set is a launch-time requirement and not a v2 feature.
	SigningKeyID string `json:"key_id,omitempty"`
	Algorithm    string `json:"algorithm,omitempty"`

	// The section 3.3 join, and the rest of section 5.1. These are values
	// copied into the manifest at build time by whatever assembles the
	// bundle; they are never computed here, and internal/update never
	// imports internal/release to obtain them (dp-v2-021 d12).
	BundleDigest    string `json:"bundle_digest,omitempty"`
	CandidateID     string `json:"candidate_id,omitempty"`
	ContractRelease string `json:"contract_release,omitempty"`
	ContractDigest  string `json:"contract_digest,omitempty"`
	RunnerAPIMin    int    `json:"runner_api_min,omitempty"`
	RunnerAPIMax    int    `json:"runner_api_max,omitempty"`

	// Roles names the bundle roles this version carries. It exists so the
	// launcher can refuse a bundle that claims to install the launcher.
	Roles []string `json:"roles,omitempty"`
}

// Interval is the closed canonical-schema interval a binary can operate
// against. It is the coexist mechanism of section 7, not a validation
// detail.
func (m Manifest) Interval() (int, int) { return m.SchemaMin, m.SchemaMax }

// Covers reports whether this version can operate against schema.
func (m Manifest) Covers(schema int) bool {
	return schema >= m.SchemaMin && schema <= m.SchemaMax
}

// Bundle is what a signer produces: the manifest bytes exactly as signed,
// the binary, and the detached signature over manifest || sha256(binary).
type Bundle struct {
	Manifest  []byte
	Binary    []byte
	Signature []byte
}

// Verify checks a bundle against the machine's accepted signing identities.
//
// The manifest is decoded before the signature is checked, because the
// manifest is what names the key id that selects the verifying entry. Only
// that selection is taken from unverified bytes: an unknown key id, an
// algorithm the entry does not declare, or a signature that does not verify
// is refused before any other manifest value is used for anything.
func Verify(bundle Bundle, anchors AnchorSet, currentSchema int) (Manifest, error) {
	if len(bundle.Manifest) == 0 || len(bundle.Binary) == 0 {
		return Manifest{}, errors.New("incomplete update bundle")
	}
	if anchors.Len() == 0 {
		return Manifest{}, fmt.Errorf("%w: no accepted signing identity", ErrAnchorUnavailable)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(bundle.Manifest))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, errors.New("invalid module manifest")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Manifest{}, errors.New("module manifest must contain exactly one JSON value")
	}
	if !ManifestSchemaAccepted(manifest.Schema) {
		return Manifest{}, fmt.Errorf("module manifest schema %q is not in the accepted set", manifest.Schema)
	}
	entry, ok := anchors.Lookup(manifest.SigningKeyID)
	if !ok {
		return Manifest{}, fmt.Errorf("module manifest names key id %q, which is absent from this machine's trust anchor", manifest.SigningKeyID)
	}
	if manifest.Algorithm == "" || entry.Algorithm != manifest.Algorithm {
		return Manifest{}, fmt.Errorf("module manifest declares algorithm %q, which the named anchor entry does not", manifest.Algorithm)
	}
	if entry.Algorithm != AlgorithmEd25519 || len(entry.PublicKey) != ed25519.PublicKeySize {
		return Manifest{}, fmt.Errorf("anchor entry %q declares algorithm %q, which this bootstrapper cannot verify", entry.KeyID, entry.Algorithm)
	}
	digest := sha256.Sum256(bundle.Binary)
	signed := append(append([]byte(nil), bundle.Manifest...), digest[:]...)
	if !ed25519.Verify(entry.PublicKey, signed, bundle.Signature) {
		return Manifest{}, errors.New("invalid update signature")
	}
	if !versionPattern.MatchString(manifest.Version) || manifest.OS != runtime.GOOS || manifest.Architecture != runtime.GOARCH {
		return Manifest{}, errors.New("incompatible module manifest")
	}
	if manifest.BinarySHA256 != hex.EncodeToString(digest[:]) || manifest.SchemaMin <= 0 || manifest.SchemaMax < manifest.SchemaMin || !manifest.Covers(currentSchema) {
		return Manifest{}, errors.New("bundle digest or schema compatibility mismatch")
	}
	for _, role := range manifest.Roles {
		if role == BootstrapperRole {
			return Manifest{}, errors.New("bundle declares a bootstrapper role: the component that verifies releases is never installed by one")
		}
	}
	if err := verifyCoordinates(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// verifyCoordinates applies the per-schema-id field requirements: v2 must
// carry the section 3.3 join and the section 5.1 contract coordinates, and
// v1 must not carry them, so no bundle can declare the older id in order to
// escape the join.
func verifyCoordinates(manifest Manifest) error {
	switch manifest.Schema {
	case ManifestSchemaV2:
		if !digestPattern.MatchString(manifest.BundleDigest) {
			return errors.New("module manifest v2 requires a 64-hex bundle_digest naming the gated source tree")
		}
		if !digestPattern.MatchString(manifest.ContractDigest) {
			return errors.New("module manifest v2 requires a 64-hex contract_digest")
		}
		if strings.TrimSpace(manifest.CandidateID) == "" || strings.TrimSpace(manifest.ContractRelease) == "" {
			return errors.New("module manifest v2 requires candidate_id and contract_release")
		}
		if manifest.RunnerAPIMin <= 0 || manifest.RunnerAPIMax < manifest.RunnerAPIMin {
			return errors.New("module manifest v2 requires a non-empty runner API range")
		}
	case ManifestSchemaV1:
		if manifest.BundleDigest != "" || manifest.CandidateID != "" || manifest.ContractRelease != "" || manifest.ContractDigest != "" || manifest.RunnerAPIMin != 0 || manifest.RunnerAPIMax != 0 {
			return errors.New("module manifest v1 must not carry v2 coordinates")
		}
	}
	return nil
}

func validRoot(root string) bool {
	return filepath.IsAbs(root) && filepath.Clean(root) != string(filepath.Separator)
}

// VersionDir is the immutable directory one installed version occupies.
func VersionDir(root, version string) string {
	return filepath.Join(root, "versions", version)
}

// Install writes a new immutable version directory. Existing versions are
// accepted only when their binary, manifest and signature bytes match,
// making retries idempotent. The signature is persisted alongside the
// manifest so that every later launch can re-run exactly this verification
// against the bytes then on disk (section 4.2 step 2).
func Install(root string, bundle Bundle, anchors AnchorSet, currentSchema int) (Manifest, error) {
	manifest, err := Verify(bundle, anchors, currentSchema)
	if err != nil {
		return Manifest{}, err
	}
	if !validRoot(root) {
		return Manifest{}, errors.New("update root must be an explicit absolute directory")
	}
	versions := filepath.Join(root, "versions")
	target := filepath.Join(versions, manifest.Version)
	if err := os.MkdirAll(versions, 0o700); err != nil {
		return Manifest{}, err
	}
	if info, statErr := os.Lstat(target); statErr == nil {
		if !info.IsDir() {
			return Manifest{}, errors.New("existing version target is not a directory")
		}
		existing, readErr := os.ReadFile(filepath.Join(target, "runner"))
		existingManifest, manifestErr := os.ReadFile(filepath.Join(target, "manifest.json"))
		existingSignature, signatureErr := os.ReadFile(filepath.Join(target, "signature"))
		if readErr != nil || manifestErr != nil || signatureErr != nil ||
			sha256.Sum256(existing) != sha256.Sum256(bundle.Binary) ||
			!bytes.Equal(existingManifest, bundle.Manifest) ||
			!bytes.Equal(existingSignature, bundle.Signature) {
			return Manifest{}, errors.New("existing immutable version differs")
		}
		return manifest, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Manifest{}, statErr
	}
	staging, err := os.MkdirTemp(versions, ".install-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(staging)
	if err := os.WriteFile(filepath.Join(staging, "runner"), bundle.Binary, 0o500); err != nil {
		return Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(staging, "manifest.json"), bundle.Manifest, 0o400); err != nil {
		return Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(staging, "signature"), bundle.Signature, 0o400); err != nil {
		return Manifest{}, err
	}
	if err := os.Rename(staging, target); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// ReadInstalledBundle reads back the three files one installed version
// occupies, in exactly the shape Verify consumes.
func ReadInstalledBundle(root, version string) (Bundle, error) {
	if !validRoot(root) || !versionPattern.MatchString(version) {
		return Bundle{}, errors.New("invalid installed version reference")
	}
	dir := VersionDir(root, version)
	manifest, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Bundle{}, err
	}
	binary, err := os.ReadFile(filepath.Join(dir, "runner"))
	if err != nil {
		return Bundle{}, err
	}
	signature, err := os.ReadFile(filepath.Join(dir, "signature"))
	if err != nil {
		return Bundle{}, err
	}
	return Bundle{Manifest: manifest, Binary: binary, Signature: signature}, nil
}

// VerifyInstalled re-runs the full bundle verification over the bytes
// currently on disk. Install-time verification says nothing about the bytes
// on disk now, so this is what the launcher calls before every exec, and it
// is the same Verify: there is no weaker on-disk check.
func VerifyInstalled(root, version string, anchors AnchorSet, currentSchema int) (Manifest, error) {
	bundle, err := ReadInstalledBundle(root, version)
	if err != nil {
		return Manifest{}, err
	}
	manifest, err := Verify(bundle, anchors, currentSchema)
	if err != nil {
		return Manifest{}, err
	}
	if manifest.Version != version {
		return Manifest{}, fmt.Errorf("installed directory %q holds a manifest for version %q", version, manifest.Version)
	}
	return manifest, nil
}

// ChannelName reports whether channel is one of the two routing channels.
func ChannelName(channel string) bool { return channel == "stable" || channel == "preview" }

// Channels is every channel symlink this machine can hold. GC re-reads all
// of them, including preview, immediately before deleting anything.
var Channels = []string{"stable", "preview"}

// ResolveChannel reads root/<channel> and returns the version it points at.
// It is the code path the measured tree had none of: nothing read
// root/stable before M8's local closure.
func ResolveChannel(root, channel string) (string, error) {
	if !validRoot(root) || !ChannelName(channel) {
		return "", errors.New("invalid channel reference")
	}
	target, err := os.Readlink(filepath.Join(root, channel))
	if err != nil {
		return "", fmt.Errorf("channel %s is not routed: %w", channel, err)
	}
	dir, version := filepath.Split(filepath.Clean(target))
	if filepath.Clean(dir) != "versions" || !versionPattern.MatchString(version) {
		return "", fmt.Errorf("channel %s points at %q, which is not a versions/<version> target", channel, target)
	}
	return version, nil
}

// linkChannel performs the atomic pointer change only. Callers hold the
// policy: Switch is the only caller, and it records the change afterwards.
func linkChannel(root, channel, version string) error {
	temp := filepath.Join(root, "."+channel+".next")
	_ = os.Remove(temp)
	if err := os.Symlink(filepath.Join("versions", version), temp); err != nil {
		return err
	}
	if err := os.Rename(temp, filepath.Join(root, channel)); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}
