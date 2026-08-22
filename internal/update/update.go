// Package update implements the small, provider-independent bootstrapper
// boundary. Versions are installed side-by-side and routing changes by one
// atomic symlink rename; an existing Stable version is never overwritten.
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
)

const ManifestSchema = "agentic-loop/module-manifest/v1"

var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[a-z0-9.-]+)?$`)

type Manifest struct {
	Schema       string `json:"schema"`
	Version      string `json:"version"`
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	BinarySHA256 string `json:"binary_sha256"`
	SchemaMin    int    `json:"schema_min"`
	SchemaMax    int    `json:"schema_max"`
}

type Bundle struct {
	Manifest  []byte
	Binary    []byte
	Signature []byte
}

func Verify(bundle Bundle, publicKey ed25519.PublicKey, currentSchema int) (Manifest, error) {
	if len(publicKey) != ed25519.PublicKeySize || len(bundle.Manifest) == 0 || len(bundle.Binary) == 0 {
		return Manifest{}, errors.New("incomplete update bundle")
	}
	digest := sha256.Sum256(bundle.Binary)
	signed := append(append([]byte(nil), bundle.Manifest...), digest[:]...)
	if !ed25519.Verify(publicKey, signed, bundle.Signature) {
		return Manifest{}, errors.New("invalid update signature")
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
	if manifest.Schema != ManifestSchema || !versionPattern.MatchString(manifest.Version) || manifest.OS != runtime.GOOS || manifest.Architecture != runtime.GOARCH {
		return Manifest{}, errors.New("incompatible module manifest")
	}
	if manifest.BinarySHA256 != hex.EncodeToString(digest[:]) || manifest.SchemaMin <= 0 || manifest.SchemaMax < manifest.SchemaMin || currentSchema < manifest.SchemaMin || currentSchema > manifest.SchemaMax {
		return Manifest{}, errors.New("bundle digest or schema compatibility mismatch")
	}
	return manifest, nil
}

// Install writes a new immutable version directory. Existing versions are
// accepted only when their binary digest matches, making retries idempotent.
func Install(root string, bundle Bundle, publicKey ed25519.PublicKey, currentSchema int) (Manifest, error) {
	manifest, err := Verify(bundle, publicKey, currentSchema)
	if err != nil {
		return Manifest{}, err
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
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
		if readErr != nil || manifestErr != nil || sha256.Sum256(existing) != sha256.Sum256(bundle.Binary) || !bytes.Equal(existingManifest, bundle.Manifest) {
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
	if err := os.Rename(staging, target); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Switch atomically changes a channel pointer without deleting its prior
// target. Rollback is the same operation with the prior verified version.
func Switch(root, channel, version string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) || (channel != "stable" && channel != "preview") || !versionPattern.MatchString(version) {
		return errors.New("invalid update switch target")
	}
	target := filepath.Join(root, "versions", version)
	info, err := os.Lstat(target)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("installed version not found: %s", version)
	}
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
