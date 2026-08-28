package provider

// Provider version compatibility (V2-074, dp-v2-074 d1-d4, d9).
//
// Compatibility is TWO relations here, both named, neither collapsed into the
// other.
//
//	R1  Provider CLI version <-> ADAPTER.  SupportedCLIVersions(adapter) is a
//	    declared, closed, half-open interval per adapter. The relation is
//	    between the version and the thing that breaks, and the thing that
//	    breaks is an adapter: it is the adapter that emits the argv and the
//	    adapter whose projected envelope shape parseFixture accepts with
//	    DisallowUnknownFields. A Loop version does not parse an envelope.
//
//	R2  adapter contract <-> LOOP version.  SupportedLoopVersions(contract) is
//	    a declared interval over the Loop's own release identity, keyed to
//	    ContractVersion -- which is already the value WorkPacket.Validate and
//	    Handoff.Validate refuse a mismatch on, so it is already the contract
//	    identity that crosses a handoff.
//
// The capability's 対応version is R1's interval and its 対応Loop Version is
// R2's interval. A verdict is the CONJUNCTION of the two, and it is unknown
// whenever either input is absent. Compatibility is deliberately NOT defined
// directly between a Provider CLI version and a Loop version: two Loop
// versions carrying byte-identical adapters would then report different
// compatibility, which is false, and every Loop release that changed nothing
// about any adapter would need a new row.
//
// AUTHORITY, per fact, and no self-claim is authority anywhere (d2). The
// four-row table is quoted verbatim in docs/operations/provider-registry.md
// and in this task's evidence:
//
//	(a) which Provider CLI versions an adapter supports -- the source-declared
//	    interval in this file, and nothing a CLI reports about itself;
//	(b) which Loop versions carry that adapter contract -- the Loop's own
//	    release identity read through internal/application's existing
//	    ReleaseObserver, and not a Runner's self-report;
//	(c) what version a CLI actually is on a machine -- the owner-approved,
//	    runner session invocation policy's provider version, measured by
//	    that record's own --version verification argv, and not a version
//	    string inside a Provider response envelope;
//	(d) whether the declared support statement is TRUE of a real CLI --
//	    nothing in this repository. Only a live exercise establishes it, and
//	    V2-028 owns it.
//
// Row (d) is why nothing in this file may be read as a claim about a real CLI.
// The intervals are a declaration this repository owns; whether a real CLI at
// a version inside one actually accepts the argv and produces the envelope is
// unmeasured here and is attributed, not asserted.
//
// WHERE THE INTERVAL BOUNDS COME FROM. One stated rule, applied to all three:
// the interval starts at the minor floor of the version the adapter's surface
// was measured at -- the version the fixture provenance manifest records for
// that adapter, which is that CLI's own --version output; reading a version
// consumes no Provider usage and needs no authentication -- and ends at the
// NARROWER of
//
//   - the next boundary at which that CLI's own versioning permits a breaking
//     change (the next minor for a 0.x CLI, the next major otherwise), and
//   - the boundary beyond which this repository has measured nothing at all.
//
// codex is 0.x, so both halves give the next minor. claude's four arguments
// are live-proven wire-compatible against a real CLI by three separate
// exercises, so the second half does not narrow it and the interval runs to
// the next major. opencode's argv surface was read from help only and never
// exercised, so the second half narrows it to the measured minor line, and
// widening it is V2-028's to earn rather than this task's to assume.
//
// COMPARISON is over the numeric triple only, and the prerelease label is
// deliberately not an ordering input. Under strict semver precedence a
// prerelease sorts BELOW the release it precedes, which would put this
// repository's only release identity -- 0.1.0-baseline -- outside every
// interval whose lower bound is its own triple. The label here is a channel
// name, not an ordering claim, so ordering by it would answer a question
// nobody asked with a false no.

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

var (
	// ErrUnknownAdapter is returned for an adapter name outside the closed
	// set of three. It is never a compatibility verdict: an undeclared
	// adapter has no interval, and reporting one would be an invention.
	ErrUnknownAdapter = errors.New("adapter name is not one of the three declared adapter names")
	// ErrUnknownContract is returned for a contract identity this package
	// does not declare an interval for.
	ErrUnknownContract = errors.New("contract version is not one this package declares a supported loop interval for")
	// ErrInvalidVersionInterval is returned for an interval that is not
	// half-open and non-empty. It exists so a declaration that says nothing
	// -- an empty interval no version can be inside -- is a refusal rather
	// than a silently unsatisfiable claim.
	ErrInvalidVersionInterval = errors.New("declared version interval is not half-open and non-empty")
)

// declaredVersionShape is the semver shape a declared or measured version must
// match to be comparable at all. It is re-declared here as a literal rather
// than imported: internal/provider must stay a leaf (a source guard forbids
// importing internal/application, internal/domain, internal/runner and
// internal/quota), and internal/application declares the same shape for the
// Runner version report. The two declarations are pinned against each other by
// a test in internal/application rather than shared by an import, which is the
// idiom this repository already uses three times over.
var declaredVersionShape = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[a-z0-9.-]+)?$`)

// VersionInterval is a half-open interval of versions: From is included, Until
// is excluded. Half-open rather than closed on both ends because the excluded
// upper bound is the version at which the declaration stops being a claim, and
// a closed upper bound would have to name the last version that works -- which
// nobody can know before it exists.
type VersionInterval struct {
	From  string `json:"from"`
	Until string `json:"until"`
}

// Validate refuses an interval that is not half-open and non-empty.
func (i VersionInterval) Validate() error {
	from, fromParsed := parseDeclaredVersion(i.From)
	until, untilParsed := parseDeclaredVersion(i.Until)
	if !fromParsed || !untilParsed {
		return ErrInvalidVersionInterval
	}
	if compareVersionTriples(from, until) >= 0 {
		return ErrInvalidVersionInterval
	}
	return nil
}

// Contains reports whether version lies inside the half-open interval, and
// whether the question could be answered at all. A version that is empty or
// does not match the declared semver shape makes the second return false: that
// is unknown, and unknown is never rounded to either answer.
func (i VersionInterval) Contains(version string) (inside bool, known bool) {
	value, parsed := parseDeclaredVersion(version)
	if !parsed {
		return false, false
	}
	from, fromParsed := parseDeclaredVersion(i.From)
	until, untilParsed := parseDeclaredVersion(i.Until)
	if !fromParsed || !untilParsed {
		return false, false
	}
	if compareVersionTriples(value, from) < 0 {
		return false, true
	}
	if compareVersionTriples(value, until) >= 0 {
		return false, true
	}
	return true, true
}

// supportedCLIVersions is R1: the declared interval per adapter. It is read
// from source and from nowhere else. No code path in this package assigns an
// interval from parsed fixture bytes, and the fixture manifest's own
// cli_version_declared field is never the authority for an interval -- it is
// the measured value the interval is asserted to CONTAIN, which is the
// opposite direction of trust.
var supportedCLIVersions = map[string]VersionInterval{
	// 0.x: the CLI's own versioning permits a breaking change at every minor,
	// and the argv surface was read from `codex exec --help` at 0.149.1.
	"codex": {From: "0.149.0", Until: "0.150.0"},
	// The four arguments are live-proven wire-compatible against a real CLI at
	// 2.1.241 by three separate exercises, so the declaration runs to the next
	// major, which is the first place a post-1.0 CLI may break.
	"claude": {From: "2.1.0", Until: "3.0.0"},
	// Post-1.0, but the argv surface was read from `opencode run --help` only
	// and never exercised, so the declaration stops at the measured minor line
	// instead of at the boundary semver would permit.
	"opencode": {From: "1.18.0", Until: "1.19.0"},
}

// supportedLoopVersions is R2: the declared interval of Loop release
// identities that carry one adapter contract. It is keyed to ContractVersion
// because that is already the identity a handoff refuses a mismatch on.
//
// The bound is the Loop's own next major. ContractVersion is "v1"; the
// repository's release identity is pre-1.0, and the first release permitted to
// change a declared contract identity is its own major.
var supportedLoopVersions = map[string]VersionInterval{
	ContractVersion: {From: "0.1.0", Until: "1.0.0"},
}

// SupportedCLIVersions returns R1's declared interval for one adapter.
func SupportedCLIVersions(adapterName string) (VersionInterval, error) {
	interval, declared := supportedCLIVersions[adapterName]
	if !declared {
		return VersionInterval{}, ErrUnknownAdapter
	}
	return interval, nil
}

// SupportedLoopVersions returns R2's declared interval for one contract
// identity.
func SupportedLoopVersions(contractVersion string) (VersionInterval, error) {
	interval, declared := supportedLoopVersions[contractVersion]
	if !declared {
		return VersionInterval{}, ErrUnknownContract
	}
	return interval, nil
}

// CompatibilityVerdict is the closed set of verdicts. unknown is a sibling
// member of the same set as compatible, not a default and not an absence: an
// input that is missing produces unknown, and no code path here rounds unknown
// to either of the other two.
type CompatibilityVerdict string

const (
	// VerdictCompatible means the measured version lies inside the declared
	// interval. It is a statement about a DECLARATION, never about a real CLI.
	VerdictCompatible CompatibilityVerdict = "compatible"
	// VerdictIncompatible means the measured version lies outside it.
	VerdictIncompatible CompatibilityVerdict = "incompatible"
	// VerdictUnknown means an input was absent or unreadable. It is first
	// class, and rounding it to either neighbour would answer a question on an
	// absence of information.
	VerdictUnknown CompatibilityVerdict = "unknown"
)

// CompatibilityVerdicts is the closed set in declaration order. A source guard
// compares it against the CompatibilityVerdict constants read from this
// package's own AST, so a verdict added without being listed -- or a set that
// spells compatible without unknown beside it -- fails a test.
func CompatibilityVerdicts() []CompatibilityVerdict {
	return []CompatibilityVerdict{VerdictCompatible, VerdictIncompatible, VerdictUnknown}
}

// CLICompatibility is R1's verdict for one adapter and one measured CLI
// version. An empty or malformed version is unknown.
func CLICompatibility(adapterName, measuredVersion string) (CompatibilityVerdict, error) {
	interval, err := SupportedCLIVersions(adapterName)
	if err != nil {
		return VerdictUnknown, err
	}
	return verdictFor(interval, measuredVersion), nil
}

// LoopCompatibility is R2's verdict for one contract identity and one observed
// Loop release identity. An absent Loop version is unknown, which is a state
// this repository genuinely reaches: a process given no explicit release
// source root can report no release version at all.
func LoopCompatibility(contractVersion, observedLoopVersion string) (CompatibilityVerdict, error) {
	interval, err := SupportedLoopVersions(contractVersion)
	if err != nil {
		return VerdictUnknown, err
	}
	return verdictFor(interval, observedLoopVersion), nil
}

// Compatibility is the conjunction of the two relations, and it is unknown
// whenever either side is unknown. incompatible wins over unknown, because an
// input measured outside a declared interval is information and must not be
// diluted by the absence of the other one.
func Compatibility(adapterName, contractVersion, measuredCLIVersion, observedLoopVersion string) (CompatibilityVerdict, error) {
	cli, err := CLICompatibility(adapterName, measuredCLIVersion)
	if err != nil {
		return VerdictUnknown, err
	}
	loop, err := LoopCompatibility(contractVersion, observedLoopVersion)
	if err != nil {
		return VerdictUnknown, err
	}
	return ConjoinVerdicts(cli, loop), nil
}

// ConjoinVerdicts is the declared conjunction over the closed verdict set. It
// is exported so the same rule is applied wherever two verdicts meet, rather
// than restated per caller.
func ConjoinVerdicts(first, second CompatibilityVerdict) CompatibilityVerdict {
	if first == VerdictIncompatible || second == VerdictIncompatible {
		return VerdictIncompatible
	}
	if first == VerdictUnknown || second == VerdictUnknown {
		return VerdictUnknown
	}
	return VerdictCompatible
}

func verdictFor(interval VersionInterval, version string) CompatibilityVerdict {
	inside, known := interval.Contains(version)
	if !known {
		return VerdictUnknown
	}
	if inside {
		return VerdictCompatible
	}
	return VerdictIncompatible
}

// parseDeclaredVersion reads the numeric triple out of a version that matches
// the declared shape. The prerelease label is accepted and then discarded: see
// the file comment for why it is not an ordering input here.
func parseDeclaredVersion(version string) ([3]int64, bool) {
	var out [3]int64
	if version == "" || !declaredVersionShape.MatchString(version) {
		return out, false
	}
	core := version
	if dash := strings.IndexByte(core, '-'); dash >= 0 {
		core = core[:dash]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return out, false
	}
	for index, part := range parts {
		value, err := strconv.ParseInt(part, 10, 64)
		if err != nil || value < 0 {
			return out, false
		}
		out[index] = value
	}
	return out, true
}

func compareVersionTriples(left, right [3]int64) int {
	for index := 0; index < 3; index++ {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	return 0
}
