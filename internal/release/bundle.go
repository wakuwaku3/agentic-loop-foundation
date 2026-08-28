// Bundle member assembly for the M5 local Release Candidate.
//
// A Release Candidate's identity is not a caller-supplied digest: it is the
// canonical manifest computed from the bytes actually present in a source
// tree at seven fixed roles (dp-v2-021 d3). No function in this file accepts
// a digest as input; every digest here is an output of reading files.
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// Role names the seven bundle roles fixed by dp-v2-021 d3. Adding an eighth
// role is a contract-level decision, not a local implementation change.
type Role string

const (
	RoleContract               Role = "contract"
	RoleSchema                 Role = "schema"
	RoleAPIContract            Role = "api-contract"
	RoleImplementationManifest Role = "implementation-manifest"
	RoleMigration              Role = "migration"
	RoleConfiguration          Role = "configuration"
	RoleDocumentation          Role = "documentation"
)

// Member is one declared bundle entry: a role, a repository-relative path
// using forward slashes, and the sha256 of that path's bytes.
type Member struct {
	Role   Role
	Path   string
	SHA256 string
}

type roleSpec struct {
	role  Role
	globs []string
	files []string
}

// roleSpecs is the fixed seven-role partition. globs enumerate every regular
// file under a prefix (optionally restricted to a suffix); files declares
// individual paths that must exist. Both forms refuse a symlink and a path
// escaping the root.
var roleSpecs = []roleSpec{
	{role: RoleContract, globs: []string{"contracts/release-contract/**"}},
	{role: RoleSchema, globs: []string{"contracts/schemas/**"}},
	{role: RoleAPIContract, globs: []string{"contracts/openapi/**"}},
	{role: RoleImplementationManifest, files: []string{"ci/components.json", "go.mod", "go.sum", "devbox.lock"}},
	{role: RoleMigration, files: []string{"firestore.indexes.json"}},
	{role: RoleConfiguration, files: []string{"devbox.json", "firebase.json"}},
	{role: RoleDocumentation, globs: []string{"docs/preview/**.md", "docs/stable/**.md"}},
}

// UnguardedMembers documents, per dp-v2-021 d5, which bundle members the
// release component's selective-CI evidence key does not cover. A test
// recomputes the evidence-key closure from ci/components.json and asserts
// every bundle member is either inside it or named here with a reason; see
// escalation E1 in docs/operations/release-local.md.
var UnguardedMembers = []UnguardedMember{
	{Glob: "docs/preview/**.md", Reason: "docs has no public_contracts, so nothing couples it to the release component's contract_dependencies (E1)"},
	{Glob: "docs/stable/**.md", Reason: "docs has no public_contracts, so nothing couples it to the release component's contract_dependencies (E1)"},
	{Glob: "firestore.indexes.json", Reason: "declared only under the store-firestore component's roots, not release's closure (E1/E4)"},
	{Glob: "firebase.json", Reason: "declared only under the environment component's roots, not release's closure (E1/E4)"},
}

type UnguardedMember struct {
	Glob   string
	Reason string
}

func globMatch(pattern, path string) bool {
	pattern = filepath.ToSlash(pattern)
	path = filepath.ToSlash(path)
	if idx := strings.Index(pattern, "**"); idx >= 0 {
		prefix, suffix := pattern[:idx], pattern[idx+2:]
		return strings.HasPrefix(path, prefix) && strings.HasSuffix(path, suffix) && len(path) > len(prefix)
	}
	return pattern == path
}

// pathEscapesRoot reports whether a repository-relative path, once cleaned,
// would resolve outside the repository root.
func pathEscapesRoot(path string) bool {
	clean := filepath.ToSlash(filepath.Clean(path))
	if filepath.IsAbs(clean) {
		return true
	}
	return clean == ".." || strings.HasPrefix(clean, "../")
}

// NewMember hashes data and refuses a path that escapes the repository root.
// It performs no filesystem access; it exists so a test can exercise the
// path-escape refusal and the digest framing without a real tree.
func NewMember(role Role, path string, data []byte) (Member, error) {
	if pathEscapesRoot(path) {
		return Member{}, fmt.Errorf("%w: %s", ErrMemberPathEscapesRoot, path)
	}
	sum := sha256.Sum256(data)
	return Member{Role: role, Path: filepath.ToSlash(filepath.Clean(path)), SHA256: hex.EncodeToString(sum[:])}, nil
}

func sortMembers(members []Member) []Member {
	out := append([]Member(nil), members...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// frame renders the canonical digest input: role\x00path\x00hexdigest\n for
// every member, sorted by (role, path), per dp-v2-021 d3.
func frame(members []Member) []byte {
	sorted := sortMembers(members)
	var buf strings.Builder
	for _, m := range sorted {
		buf.WriteString(string(m.Role))
		buf.WriteByte(0)
		buf.WriteString(m.Path)
		buf.WriteByte(0)
		buf.WriteString(m.SHA256)
		buf.WriteByte('\n')
	}
	return []byte(buf.String())
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// BundleDigestFromMembers is sha256 over the framing of every member. It is
// independent of the order members are supplied in.
func BundleDigestFromMembers(members []Member) string { return digestOf(frame(members)) }

// DocsFraming returns the framing bytes restricted to the documentation
// role. It is exposed so callers (CompileContract's docs argument) and
// DocsDigestFromMembers derive from exactly the same bytes, per dp-v2-021 d3.
func DocsFraming(members []Member) []byte {
	var docsOnly []Member
	for _, m := range members {
		if m.Role == RoleDocumentation {
			docsOnly = append(docsOnly, m)
		}
	}
	return frame(docsOnly)
}

// DocsDigestFromMembers is sha256 over DocsFraming.
func DocsDigestFromMembers(members []Member) string { return digestOf(DocsFraming(members)) }

var (
	ErrDeclaredMemberMissing = errors.New("declared bundle member does not exist")
	ErrMemberIsSymlink       = errors.New("bundle member is a symlink")
	ErrRoleHasNoMembers      = errors.New("bundle role resolved to zero members")
	ErrMemberPathEscapesRoot = errors.New("bundle member path escapes repository root")
)

// assembleMembers walks root once and returns every member across all seven
// roles, refusing a symlink, a missing declared file, a path escaping root,
// or a role with zero resolved members.
func assembleMembers(root string) ([]Member, error) {
	root = filepath.Clean(root)
	var members []Member
	counts := make(map[Role]int, len(roleSpecs))

	readMember := func(role Role, rel string, full string) error {
		if pathEscapesRoot(rel) {
			return fmt.Errorf("%w: %s", ErrMemberPathEscapesRoot, rel)
		}
		lst, err := os.Lstat(full)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("%w: %s", ErrDeclaredMemberMissing, rel)
			}
			return err
		}
		if lst.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrMemberIsSymlink, rel)
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		members = append(members, Member{Role: role, Path: filepath.ToSlash(rel), SHA256: digestOf(data)})
		counts[role]++
		return nil
	}

	for _, spec := range roleSpecs {
		for _, f := range spec.files {
			if err := readMember(spec.role, f, filepath.Join(root, filepath.FromSlash(f))); err != nil {
				return nil, err
			}
		}
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		isSymlink := d.Type()&fs.ModeSymlink != 0
		for _, spec := range roleSpecs {
			if len(spec.globs) == 0 {
				continue
			}
			matched := false
			for _, g := range spec.globs {
				if globMatch(g, rel) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			if isSymlink {
				return fmt.Errorf("%w: %s", ErrMemberIsSymlink, rel)
			}
			return readMember(spec.role, rel, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	for _, spec := range roleSpecs {
		if counts[spec.role] == 0 {
			return nil, fmt.Errorf("%w: role %s", ErrRoleHasNoMembers, spec.role)
		}
	}
	return members, nil
}

// AssembledBundle is the result of reading a source tree: the member list
// and every digest derived from it, plus the compiled Release Contract.
type AssembledBundle struct {
	Members        []Member
	BundleDigest   string
	ContractDigest string
	DocsDigest     string
	Contract       CompiledContract
}

// AssembleFromRoot reads every bundle member from root and derives all
// digests from their bytes. No digest is accepted as a parameter.
func AssembleFromRoot(root string) (AssembledBundle, error) {
	members, err := assembleMembers(root)
	if err != nil {
		return AssembledBundle{}, err
	}
	contractPath := filepath.Join(root, "contracts", "release-contract", "foundation.json")
	contractBytes, err := os.ReadFile(contractPath)
	if err != nil {
		return AssembledBundle{}, fmt.Errorf("read release contract: %w", err)
	}
	docsFraming := DocsFraming(members)
	compiled, err := CompileContract(contractBytes, docsFraming)
	if err != nil {
		return AssembledBundle{}, err
	}
	return AssembledBundle{
		Members:        members,
		BundleDigest:   BundleDigestFromMembers(members),
		ContractDigest: compiled.Digest,
		DocsDigest:     compiled.DocsDigest,
		Contract:       compiled,
	}, nil
}

// DriftError names the member whose bytes no longer agree with the source
// tree, or whose presence in the source tree is unrecorded.
type DriftError struct {
	Role   Role
	Path   string
	Reason string
}

func (e *DriftError) Error() string {
	return fmt.Sprintf("source drift: role=%s path=%s: %s", e.Role, e.Path, e.Reason)
}

func memberKey(m Member) string { return string(m.Role) + "\x00" + m.Path }

// VerifySource re-assembles root and refuses if any recorded member's bytes
// no longer match, any recorded member is absent from the tree, or the tree
// now contains a member the recorded set does not know about. The bundle's
// digest is therefore re-verified against source bytes on every promotion
// attempt; no caller-asserted digest is trusted (dp-v2-021 d2, d4 L1).
func VerifySource(recorded []Member, root string) error {
	fresh, err := assembleMembers(root)
	if err != nil {
		return err
	}
	freshByKey := make(map[string]Member, len(fresh))
	for _, m := range fresh {
		freshByKey[memberKey(m)] = m
	}
	recByKey := make(map[string]Member, len(recorded))
	for _, m := range recorded {
		recByKey[memberKey(m)] = m
	}
	for key, rm := range recByKey {
		fm, ok := freshByKey[key]
		if !ok {
			return &DriftError{Role: rm.Role, Path: rm.Path, Reason: "recorded member is no longer present in the source tree"}
		}
		if fm.SHA256 != rm.SHA256 {
			return &DriftError{Role: rm.Role, Path: rm.Path, Reason: "member bytes changed since assembly"}
		}
	}
	for key, fm := range freshByKey {
		if _, ok := recByKey[key]; !ok {
			return &DriftError{Role: fm.Role, Path: fm.Path, Reason: "source tree contains a member absent from the recorded bundle"}
		}
	}
	return nil
}

// ContractMemberPath is the single member ContractDigest is defined over:
// sha256 of contracts/release-contract/foundation.json bytes (dp-v2-021 d3).
const ContractMemberPath = "contracts/release-contract/foundation.json"

var ErrCandidateDigestMismatch = errors.New("candidate digest does not match the source-derived value")

// VerifyCandidateDigests refuses a candidate whose recorded BundleDigest,
// DocsDigest or ContractDigest disagree with the digests computed from
// members. This is what closes the defect measured in dp-v2-021 d2: a
// caller can set candidate.DocsDigest to any string it likes and, before
// this check existed, a matching evidence[i].DocsDigest was enough to
// satisfy domain.CanPromote's internal-consistency check even though
// neither digest agreed with the source tree. members must already have
// been confirmed to match the source tree (VerifySource) before calling
// this; it does not re-read the filesystem.
func VerifyCandidateDigests(candidate domain.ReleaseCandidate, members []Member) error {
	if want := BundleDigestFromMembers(members); candidate.BundleDigest != want {
		return fmt.Errorf("%w: BundleDigest %q, source-derived %q", ErrCandidateDigestMismatch, candidate.BundleDigest, want)
	}
	if want := DocsDigestFromMembers(members); candidate.DocsDigest != want {
		return fmt.Errorf("%w: DocsDigest %q, source-derived %q", ErrCandidateDigestMismatch, candidate.DocsDigest, want)
	}
	for _, m := range members {
		if m.Role == RoleContract && m.Path == ContractMemberPath {
			if candidate.ContractDigest != m.SHA256 {
				return fmt.Errorf("%w: ContractDigest %q, source-derived %q", ErrCandidateDigestMismatch, candidate.ContractDigest, m.SHA256)
			}
		}
	}
	return nil
}

// VerifyCandidateAgainstContract refuses any candidate whose capability set
// is not exactly the compiled contract's capability set, in contract order
// (dp-v2-021 d6). The candidate's capability set must be derived, not
// supplied, so this is the boundary that enforces that.
func VerifyCandidateAgainstContract(candidate domain.ReleaseCandidate, contract CompiledContract) error {
	if len(candidate.Capabilities) != len(contract.Capabilities) {
		return fmt.Errorf("candidate declares %d capabilities, contract declares %d", len(candidate.Capabilities), len(contract.Capabilities))
	}
	for i, id := range contract.Capabilities {
		if candidate.Capabilities[i] != id {
			return fmt.Errorf("candidate capability set diverges from contract at position %d: candidate has %q, contract declares %q", i, candidate.Capabilities[i], id)
		}
	}
	return nil
}

func computeEvidenceDigest(evidence []domain.CapabilityEvidence) string {
	sorted := append([]domain.CapabilityEvidence(nil), evidence...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Capability < sorted[j].Capability })
	var buf strings.Builder
	for _, e := range sorted {
		buf.WriteString(e.Capability)
		buf.WriteByte(0)
		buf.WriteString(e.Digest)
		buf.WriteByte('\n')
	}
	return digestOf([]byte(buf.String()))
}

// CandidateInput is everything about a Release Candidate that assembly
// cannot derive from source bytes: identity, operational evidence, and
// control/fencing state.
type CandidateInput struct {
	ReleaseID               domain.ReleaseID
	CandidateID             domain.ReleaseID
	CandidateDigest         string
	Version                 domain.Version
	Status                  domain.ReleaseStatus
	Evidence                []domain.CapabilityEvidence
	CapabilityTargets       map[string]domain.CapabilityTarget
	AffectedProviders       []string
	RollbackEvidence        bool
	ResumeEvidence          bool
	ExpectedControlRevision domain.Revision
	FencingToken            domain.FencingToken
}

// AssembleCandidate reads root and returns both the assembled bundle and a
// ReleaseCandidate whose Capabilities, BundleDigest, ContractDigest and
// DocsDigest are derived from source bytes, never supplied by the caller
// (dp-v2-021 d6). Evidence freshness and completeness are still enforced by
// domain.ReleaseCandidate.CanPromote and by VerifyCandidateAgainstContract.
func AssembleCandidate(root string, input CandidateInput) (AssembledBundle, domain.ReleaseCandidate, error) {
	assembled, err := AssembleFromRoot(root)
	if err != nil {
		return AssembledBundle{}, domain.ReleaseCandidate{}, err
	}
	candidate := domain.ReleaseCandidate{
		ID:                      input.ReleaseID,
		CandidateID:             input.CandidateID,
		CandidateDigest:         input.CandidateDigest,
		Version:                 input.Version,
		Status:                  input.Status,
		Capabilities:            append([]string(nil), assembled.Contract.Capabilities...),
		Evidence:                input.Evidence,
		RollbackEvidence:        input.RollbackEvidence,
		ResumeEvidence:          input.ResumeEvidence,
		ExpectedControlRevision: input.ExpectedControlRevision,
		FencingToken:            input.FencingToken,
		BundleDigest:            assembled.BundleDigest,
		ContractDigest:          assembled.ContractDigest,
		DocsDigest:              assembled.DocsDigest,
		EvidenceDigest:          computeEvidenceDigest(input.Evidence),
		CapabilityTargets:       input.CapabilityTargets,
		AffectedProviders:       append([]string(nil), input.AffectedProviders...),
	}
	return assembled, candidate, nil
}
