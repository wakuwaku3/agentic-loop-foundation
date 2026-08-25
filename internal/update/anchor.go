// Trust anchor resolution for the Bootstrapper (docs/operations/self-update.md
// sections 3.1 and 3.2).
//
// The measured defect this file removes: `bootstrap install --public-key
// <path>` asked its caller for the Bootstrapper's own trust anchor, so a
// caller who could choose the key could sign anything with a key of their
// own. The anchor is therefore no longer an argument. It is resolved from a
// fixed location derived from the machine root, and the resolution refuses
// -- before `versions/` is touched and before any child process is started
// -- rather than degrading to "verify with whatever was supplied".
//
// The refusal shape is the one internal/runner/confinement.go already
// established for the Runner side: one exported sentinel that callers test
// with errors.Is, a typed refusal reason, and no silent fallback.
package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// AlgorithmEd25519 is the only signature algorithm this Bootstrapper can
// verify. The anchor format nonetheless records an algorithm per entry so a
// second algorithm can be added as an accepted entry during a signer
// migration (section 3.1, escalation E3) without changing this file; an
// entry whose algorithm this build cannot verify is kept in the set and
// refused at verification time, never silently treated as Ed25519.
const AlgorithmEd25519 = "ed25519"

// anchorDirName and anchorFileName spell the fixed, root-relative anchor
// location. They are not configurable: a caller who could name the anchor
// file could substitute the key, which is the whole defect being removed.
const (
	anchorDirName  = "trust"
	anchorFileName = "release-keys"
)

// AnchorPath is the fixed absolute path of the machine trust anchor for a
// machine whose bootstrap root is root.
func AnchorPath(root string) string {
	return filepath.Join(root, anchorDirName, anchorFileName)
}

// ErrAnchorUnavailable reports that the machine trust anchor could not be
// resolved into a usable set of accepted signing identities. Callers must
// treat it as a hard, reported failure: there is no default key, no
// --insecure flag and no fallback to an unverified bundle.
var ErrAnchorUnavailable = errors.New("update: the machine trust anchor is unusable, so no bundle can be verified")

// AnchorRefusal names which of the enumerated anchor refusals fired. The
// first six are section 3.2's list in order; AnchorMalformed is a seventh
// refusal this implementation adds because a line that does not parse is a
// fail-closed condition rather than a line to skip.
type AnchorRefusal string

const (
	AnchorAbsent       AnchorRefusal = "anchor-absent"
	AnchorNotRegular   AnchorRefusal = "anchor-not-a-regular-file"
	AnchorForeignOwner AnchorRefusal = "anchor-not-owned-by-the-invoking-user"
	AnchorModeTooWide  AnchorRefusal = "anchor-mode-wider-than-0600"
	AnchorEmpty        AnchorRefusal = "anchor-empty"
	AnchorNoEntries    AnchorRefusal = "anchor-zero-accepted-entries"
	AnchorMalformed    AnchorRefusal = "anchor-malformed-entry"
)

// AnchorError is a refusal from AnchorResolver.Resolve. It unwraps to
// ErrAnchorUnavailable so callers can fail closed on the class while tests
// can assert the individual refusal.
type AnchorError struct {
	Refusal AnchorRefusal
	Path    string
	Detail  string
}

func (e *AnchorError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("%s: %s at %s", ErrAnchorUnavailable.Error(), e.Refusal, e.Path)
	}
	return fmt.Sprintf("%s: %s at %s: %s", ErrAnchorUnavailable.Error(), e.Refusal, e.Path, e.Detail)
}

func (e *AnchorError) Unwrap() error { return ErrAnchorUnavailable }

// AnchorEntry is one accepted signing identity. PublicKey is populated only
// for AlgorithmEd25519; an entry carrying another algorithm keeps its raw
// material so the set stays a set, and is refused at verification time.
type AnchorEntry struct {
	KeyID     string
	Algorithm string
	PublicKey ed25519.PublicKey
	raw       []byte
}

// AnchorSet is the machine's accepted signing identities, keyed by key id.
// It is a set rather than a single key so rotation across two machines that
// are never shared is add-then-remove instead of a flag day (section 3.1).
type AnchorSet struct {
	entries []AnchorEntry
}

// NewAnchorSet builds a set from explicit entries. It is the seam tests use
// instead of writing an anchor file, and the seam a future signer migration
// would use; production always builds the set through AnchorResolver.
func NewAnchorSet(entries ...AnchorEntry) AnchorSet {
	return AnchorSet{entries: append([]AnchorEntry(nil), entries...)}
}

// Ed25519Anchor is the common case: one accepted Ed25519 identity.
func Ed25519Anchor(keyID string, publicKey ed25519.PublicKey) AnchorEntry {
	return AnchorEntry{KeyID: keyID, Algorithm: AlgorithmEd25519, PublicKey: publicKey, raw: publicKey}
}

// Len is the number of accepted entries.
func (s AnchorSet) Len() int { return len(s.entries) }

// KeyIDs lists the accepted key ids in file order.
func (s AnchorSet) KeyIDs() []string {
	out := make([]string, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.KeyID)
	}
	return out
}

// Lookup selects the entry a manifest's key_id names.
func (s AnchorSet) Lookup(keyID string) (AnchorEntry, bool) {
	for _, e := range s.entries {
		if e.KeyID == keyID {
			return e, true
		}
	}
	return AnchorEntry{}, false
}

// AnchorResolver resolves the fixed-path anchor for one machine root.
// InvokingUID is injected rather than read inside Resolve so the ownership
// refusal is reachable in a test without root: a test resolves an anchor it
// owns while declaring a different invoking uid, which drives exactly the
// comparison production performs against os.Getuid().
type AnchorResolver struct {
	Root        string
	InvokingUID int
}

// NewAnchorResolver is the production constructor: the fixed path under
// root, checked against the real invoking uid.
func NewAnchorResolver(root string) AnchorResolver {
	return AnchorResolver{Root: root, InvokingUID: os.Getuid()}
}

// Resolve returns the accepted entry set, or one of the enumerated
// refusals. It reads exactly one file and creates nothing.
func (r AnchorResolver) Resolve() (AnchorSet, error) {
	if !filepath.IsAbs(r.Root) || filepath.Clean(r.Root) == string(filepath.Separator) {
		return AnchorSet{}, fmt.Errorf("%w: update root must be an explicit absolute directory", ErrAnchorUnavailable)
	}
	path := AnchorPath(r.Root)
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AnchorSet{}, &AnchorError{Refusal: AnchorAbsent, Path: path}
		}
		return AnchorSet{}, fmt.Errorf("%w: stat %s: %v", ErrAnchorUnavailable, path, err)
	}
	// Lstat, not Stat: a symlink is not a regular file here even when its
	// target is one, because the anchor's identity must be the fixed path
	// itself and not something a link can retarget.
	if !info.Mode().IsRegular() {
		return AnchorSet{}, &AnchorError{Refusal: AnchorNotRegular, Path: path, Detail: info.Mode().String()}
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return AnchorSet{}, fmt.Errorf("%w: %s: ownership is not observable on this platform", ErrAnchorUnavailable, path)
	}
	if int(stat.Uid) != r.InvokingUID {
		return AnchorSet{}, &AnchorError{Refusal: AnchorForeignOwner, Path: path, Detail: fmt.Sprintf("owner uid %d, invoking uid %d", stat.Uid, r.InvokingUID)}
	}
	if perm := info.Mode().Perm(); perm&^0o600 != 0 {
		return AnchorSet{}, &AnchorError{Refusal: AnchorModeTooWide, Path: path, Detail: fmt.Sprintf("mode %#o", perm)}
	}
	if info.Size() == 0 {
		return AnchorSet{}, &AnchorError{Refusal: AnchorEmpty, Path: path}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return AnchorSet{}, fmt.Errorf("%w: read %s: %v", ErrAnchorUnavailable, path, err)
	}
	set, err := parseAnchorEntries(path, string(data))
	if err != nil {
		return AnchorSet{}, err
	}
	if set.Len() == 0 {
		return AnchorSet{}, &AnchorError{Refusal: AnchorNoEntries, Path: path}
	}
	return set, nil
}

// parseAnchorEntries reads the "key_id algorithm base64(pubkey)" line
// format. Blank lines and # comments carry no entry; anything else that is
// not exactly three fields, or that repeats a key id, is a refusal.
func parseAnchorEntries(path, data string) (AnchorSet, error) {
	var entries []AnchorEntry
	seen := map[string]bool{}
	for i, line := range strings.Split(data, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) != 3 {
			return AnchorSet{}, &AnchorError{Refusal: AnchorMalformed, Path: path, Detail: fmt.Sprintf("line %d has %d fields, want key_id algorithm base64(pubkey)", i+1, len(fields))}
		}
		keyID, algorithm := fields[0], fields[1]
		if seen[keyID] {
			return AnchorSet{}, &AnchorError{Refusal: AnchorMalformed, Path: path, Detail: fmt.Sprintf("line %d repeats key id %q", i+1, keyID)}
		}
		material, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(fields[2], "="))
		if err != nil || len(material) == 0 {
			return AnchorSet{}, &AnchorError{Refusal: AnchorMalformed, Path: path, Detail: fmt.Sprintf("line %d does not carry base64 material", i+1)}
		}
		if algorithm == AlgorithmEd25519 && len(material) != ed25519.PublicKeySize {
			return AnchorSet{}, &AnchorError{Refusal: AnchorMalformed, Path: path, Detail: fmt.Sprintf("line %d declares %s with %d bytes of material", i+1, AlgorithmEd25519, len(material))}
		}
		entry := AnchorEntry{KeyID: keyID, Algorithm: algorithm, raw: material}
		if algorithm == AlgorithmEd25519 {
			entry.PublicKey = ed25519.PublicKey(material)
		}
		seen[keyID] = true
		entries = append(entries, entry)
	}
	return AnchorSet{entries: entries}, nil
}

// FormatAnchorLine renders one anchor entry in the on-disk line format. It
// exists so a test fixture and an out-of-band replication procedure produce
// the same bytes this package parses.
func FormatAnchorLine(entry AnchorEntry) string {
	material := entry.raw
	if material == nil {
		material = entry.PublicKey
	}
	return entry.KeyID + " " + entry.Algorithm + " " + base64.RawStdEncoding.EncodeToString(material) + "\n"
}
