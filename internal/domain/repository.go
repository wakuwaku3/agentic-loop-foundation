package domain

// Repository is the Installation's registered change boundary: an
// Application's source of truth on a forge, made addressable to the Loop.
//
// This file follows the measured precedent of control.go, lease.go and
// release.go: the aggregate declares its own ValidateRepository and
// DecideRepository rather than extending model.go's generic Validate and
// Decide type switches (dp-v2-064 d3). model.go's Validate handles only
// *Requirement/*Increment/*Execution/*Lease and their value forms, and
// Decide handles only Requirement and Increment; ControlIntent, the largest
// surface in this package, appears in neither.
//
// Identity is a loop-issued opaque RepositoryID. The forge coordinates are a
// separate value object, SourceLocator, and the clone URL is never the
// identity and is never stored as a key (dp-v2-064 d2): a rename or transfer
// changes owner/name while the Repository is the same object, and some URL
// forms can carry a credential, which must never enter a canonical record.

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// RepositoryID is an opaque, loop-issued identifier. Like every other
// identifier in this package it is produced through opaqueID and its value
// is never interpreted by the domain.
type RepositoryID string

func NewRepositoryID(v string) (RepositoryID, error) { return opaqueID(RepositoryID(v)) }

func (v RepositoryID) String() string { return string(v) }

// ForgeGitHub is the only forge this Loop reaches today. It is a plain
// string so the value object stays free of any client dependency.
const ForgeGitHub = "github"

// DefaultForgeHost is the host assumed when a locator names an owner and a
// name without one.
const DefaultForgeHost = "github.com"

var (
	// ErrInvalidSourceLocator is returned when a locator cannot be reduced
	// to a (forge, host, owner, name) tuple this Loop can address.
	ErrInvalidSourceLocator = errors.New("invalid repository source locator")
	// ErrUnknownForge is returned for a forge this Loop does not reach.
	ErrUnknownForge = errors.New("unknown forge")
)

// SourceLocator is the forge coordinate of a Repository. Every field is a
// plain string: the locator carries no token, secret, password, auth or
// credential material under any spelling, which internal/domain's AST source
// guard proves mechanically rather than by review.
type SourceLocator struct {
	Forge         string `json:"forge"`
	Host          string `json:"host"`
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

// Key is the normalised uniqueness key of a Repository within an
// Installation: the (forge, owner, name) triple. Host is deliberately not
// part of it -- one forge is one namespace -- and DefaultBranch is a mutable
// observation, never part of identity.
func (l SourceLocator) Key() string {
	return l.Forge + "/" + l.Owner + "/" + l.Name
}

// Recorded reports whether this locator was actually populated, so a caller
// can distinguish an absent locator from an empty one.
func (l SourceLocator) Recorded() bool {
	return l.Forge != "" || l.Host != "" || l.Owner != "" || l.Name != ""
}

// containsPathSeparator reports whether value carries either path separator.
// An owner or a name that does is not a single path segment and therefore is
// not a repository coordinate, whatever it might parse as.
func containsPathSeparator(value string) bool {
	return strings.ContainsAny(value, "/\\")
}

// trimGitSuffix removes exactly one trailing ".git", the conventional bare
// repository suffix. It is applied to the name segment only.
func trimGitSuffix(value string) string {
	if len(value) > len(".git") && strings.HasSuffix(strings.ToLower(value), ".git") {
		return value[:len(value)-len(".git")]
	}
	return value
}

// NormalizeSourceLocator reduces a locator to its canonical comparable form:
// forge defaulted to ForgeGitHub and validated, host defaulted and
// lowercased, owner and name lowercased with a trailing ".git" stripped from
// the name, and the default branch trimmed but never lowercased (a branch
// name is case-sensitive on every forge this Loop reaches).
//
// It refuses an owner or a name that is empty or that contains a path
// separator, so a value that is not a single path segment can never become
// half of a uniqueness key.
func NormalizeSourceLocator(locator SourceLocator) (SourceLocator, error) {
	out := SourceLocator{
		Forge:         strings.ToLower(strings.TrimSpace(locator.Forge)),
		Host:          strings.ToLower(strings.TrimSpace(locator.Host)),
		Owner:         strings.ToLower(strings.TrimSpace(locator.Owner)),
		Name:          strings.ToLower(strings.TrimSpace(trimGitSuffix(strings.TrimSpace(locator.Name)))),
		DefaultBranch: strings.TrimSpace(locator.DefaultBranch),
	}
	if out.Forge == "" {
		out.Forge = ForgeGitHub
	}
	if out.Forge != ForgeGitHub {
		return SourceLocator{}, fmt.Errorf("%w: %q", ErrUnknownForge, out.Forge)
	}
	if out.Host == "" {
		out.Host = DefaultForgeHost
	}
	if containsPathSeparator(out.Host) {
		return SourceLocator{}, fmt.Errorf("%w: host %q is not a single host segment", ErrInvalidSourceLocator, out.Host)
	}
	if out.Owner == "" || containsPathSeparator(out.Owner) {
		return SourceLocator{}, fmt.Errorf("%w: owner must be one non-empty path segment", ErrInvalidSourceLocator)
	}
	if out.Name == "" || containsPathSeparator(out.Name) {
		return SourceLocator{}, fmt.Errorf("%w: name must be one non-empty path segment", ErrInvalidSourceLocator)
	}
	// A branch name may legitimately contain "/" ("feature/x") but never a
	// backslash, and never whitespace.
	if strings.Contains(out.DefaultBranch, "\\") || strings.ContainsAny(out.DefaultBranch, " \t\r\n") {
		return SourceLocator{}, fmt.Errorf("%w: default branch %q is not a branch name", ErrInvalidSourceLocator, out.DefaultBranch)
	}
	return out, nil
}

// ParseSourceLocator accepts the forms a person or a tool actually produces
// for the same repository and reduces all of them to one normalised locator:
//
//	https://github.com/Owner/Name
//	https://github.com/Owner/Name.git
//	http://github.com/Owner/Name
//	ssh://git@github.com/Owner/Name.git
//	git://github.com/Owner/Name.git
//	git@github.com:Owner/Name.git   (scp-style)
//	github.com/Owner/Name
//	Owner/Name                      (host defaulted)
//
// It parses by hand rather than with net/url, because internal/domain admits
// no network-shaped import at all and its AST source guard enforces that.
// A userinfo section is dropped before anything else is examined, so a URL
// form that carried credential material can never survive into the locator.
func ParseSourceLocator(raw string) (SourceLocator, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return SourceLocator{}, fmt.Errorf("%w: empty locator", ErrInvalidSourceLocator)
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return SourceLocator{}, fmt.Errorf("%w: locator contains whitespace", ErrInvalidSourceLocator)
	}
	for _, scheme := range []string{"https://", "http://", "ssh://", "git://", "git+ssh://"} {
		if lower := strings.ToLower(value); strings.HasPrefix(lower, scheme) {
			value = value[len(scheme):]
			break
		}
	}
	// Drop any userinfo ("git@", and equally any "user:password@" form).
	// This is the point at which a credential-bearing URL loses the
	// credential: nothing downstream ever sees it, so nothing downstream can
	// store or log it.
	if at := strings.LastIndex(value, "@"); at >= 0 {
		value = value[at+1:]
	}
	if value == "" {
		return SourceLocator{}, fmt.Errorf("%w: locator has no host or path", ErrInvalidSourceLocator)
	}
	host, path := splitLocatorHost(value)
	segments := make([]string, 0, 3)
	for _, segment := range strings.Split(path, "/") {
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	if len(segments) != 2 {
		return SourceLocator{}, fmt.Errorf("%w: expected one owner and one name segment, found %d", ErrInvalidSourceLocator, len(segments))
	}
	return NormalizeSourceLocator(SourceLocator{Forge: ForgeGitHub, Host: host, Owner: segments[0], Name: segments[1]})
}

// splitLocatorHost separates the host from the path in a scheme-less,
// userinfo-less locator. It accepts both the URL separator ("host/path") and
// the scp-style separator ("host:path"), and tolerates an explicit port
// ("host:22/path") by recognising an all-digit segment before the next "/".
//
// A leading segment is only treated as a host when it actually looks like one
// (it contains a dot, or is exactly "localhost"). That is what keeps a bare
// "Owner/Name" parsing as owner and name with a defaulted host, and keeps
// "owner/name/extra" being rejected for having three segments instead of
// being silently reinterpreted as host "owner".
func splitLocatorHost(value string) (string, string) {
	slash := strings.IndexByte(value, '/')
	colon := strings.IndexByte(value, ':')
	if colon > 0 && (slash < 0 || colon < slash) && hostLike(value[:colon]) {
		rest := value[colon+1:]
		// An explicit port is digits up to the next "/": skip it and use the
		// URL form's separator instead.
		if end := strings.IndexByte(rest, '/'); end > 0 && isAllDigits(rest[:end]) {
			return value[:colon], rest[end+1:]
		}
		if !isAllDigits(rest) {
			return value[:colon], rest
		}
		return value[:colon], ""
	}
	if slash > 0 && hostLike(value[:slash]) {
		return value[:slash], value[slash+1:]
	}
	return "", value
}

// hostLike reports whether segment can be a forge host rather than an owner.
func hostLike(segment string) bool {
	return strings.Contains(segment, ".") || strings.EqualFold(segment, "localhost")
}

func isAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// RepositoryStatus is closed at exactly two values. A Repository is never
// deleted: retire is a transition, so the rollback of a registration is
// observable rather than a disappearance (dp-v2-064 d5).
type RepositoryStatus string

const (
	RepositoryRegistered RepositoryStatus = "registered"
	RepositoryRetired    RepositoryStatus = "retired"
)

const (
	RepositoryStatusRegistered = RepositoryRegistered
	RepositoryStatusRetired    = RepositoryRetired
)

func validRepositoryStatus(s RepositoryStatus) bool {
	switch s {
	case RepositoryRegistered, RepositoryRetired:
		return true
	}
	return false
}

// Repository is Installation-owned. It is never person-owned and never
// Runner-bound: RequestedBy records who caused the registration for
// attribution only and is never interpreted, authenticated or authorized
// here, and there is deliberately no assignee, owner-identity, ACL,
// permission or runner field (dp-v2-064 d4).
type Repository struct {
	ID          RepositoryID
	Locator     SourceLocator
	Status      RepositoryStatus
	Version     Version
	RequestedBy RequestedBy
}

// ValidateRepository is this aggregate's own invariant check, the analogue of
// model.go's Validate for the types that predate this file.
func ValidateRepository(value Repository) error {
	if _, err := NewRepositoryID(value.ID.String()); err != nil {
		return err
	}
	if !validRepositoryStatus(value.Status) {
		return fmt.Errorf("unknown repository status %q", value.Status)
	}
	normalized, err := NormalizeSourceLocator(value.Locator)
	if err != nil {
		return err
	}
	if normalized != value.Locator {
		return fmt.Errorf("%w: locator is not in normalised form", ErrInvalidSourceLocator)
	}
	if value.RequestedBy.ActorType != "" && !validActorType(value.RequestedBy.ActorType) {
		return fmt.Errorf("unknown requested_by actor_type %q", value.RequestedBy.ActorType)
	}
	return nil
}

type RepositoryCommandKind string

const (
	RepositoryRegister RepositoryCommandKind = "register"
	RepositoryRetire   RepositoryCommandKind = "retire"
)

type RepositoryCommand struct {
	Kind            RepositoryCommandKind
	Actor           ActorID
	At              time.Time
	ExpectedVersion Version
}

// DecideRepository is the aggregate's pure transition function. Registration
// is the 0 -> 1 transition of a Repository that carries its identity and its
// locator but no status yet; retire is the registered -> retired transition.
// Both bump Version, so the store's optimistic concurrency check has exactly
// one thing to compare.
func DecideRepository(current Repository, command RepositoryCommand) (Repository, error) {
	if command.Actor == "" || command.At.IsZero() {
		return current, errors.New("actor and explicit timestamp are required")
	}
	if current.Version != command.ExpectedVersion {
		return current, ErrStaleVersion
	}
	next := current
	switch command.Kind {
	case RepositoryRegister:
		if current.Status != "" || current.Version != 0 {
			return current, ErrInvalidTransition
		}
		if _, err := NewRepositoryID(current.ID.String()); err != nil {
			return current, err
		}
		locator, err := NormalizeSourceLocator(current.Locator)
		if err != nil {
			return current, err
		}
		next.Locator = locator
		next.Status = RepositoryRegistered
	case RepositoryRetire:
		if err := ValidateRepository(current); err != nil {
			return current, err
		}
		if current.Status != RepositoryRegistered {
			return current, ErrInvalidTransition
		}
		next.Status = RepositoryRetired
	default:
		return current, fmt.Errorf("unknown repository command %q", command.Kind)
	}
	version, err := next.Version.Next()
	if err != nil {
		return current, err
	}
	next.Version = version
	if err := ValidateRepository(next); err != nil {
		return current, err
	}
	return next, nil
}

// RepositoryObservation is bounded forge evidence, never canonical state and
// never a command: Git and forge state enter the Loop as an Observation
// (dp-v2-064 d6). It carries no raw process output, no response body and no
// credential: only the parsed fields below ever leave the adapter.
type RepositoryObservation struct {
	RepositoryID   RepositoryID  `json:"repository_id"`
	Locator        SourceLocator `json:"locator"`
	Reachable      bool          `json:"reachable"`
	DefaultBranch  string        `json:"default_branch,omitempty"`
	CanPush        bool          `json:"can_push"`
	ForgeNodeID    string        `json:"forge_node_id,omitempty"`
	AdapterVersion string        `json:"adapter_version,omitempty"`
	Reason         string        `json:"reason,omitempty"`
	ObservedAt     time.Time     `json:"observed_at"`
}

// Recorded distinguishes a real observation from a zero value.
func (o RepositoryObservation) Recorded() bool { return !o.ObservedAt.IsZero() }

// StaleAt reports whether this observation is older than staleAfter as of
// now. Both are supplied by the caller: this package reads no clock, which
// its AST source guard proves by forbidding time.Now outright.
func (o RepositoryObservation) StaleAt(now time.Time, staleAfter time.Duration) bool {
	if !o.Recorded() {
		return true
	}
	if staleAfter <= 0 || now.IsZero() {
		return false
	}
	return now.Sub(o.ObservedAt) >= staleAfter
}

// DefaultObservationStaleAfter bounds how long a forge Observation is
// treated as current. It exists so a Runner does not spend the
// Installation's bounded Firestore write budget re-submitting telemetry on
// every heartbeat (dp-v2-064 d15).
const DefaultObservationStaleAfter = 15 * time.Minute

// ShouldObserveRepository decides whether a Runner should submit a fresh
// Observation: when none exists, when the stored one is stale as of the
// injected now, or when the locator it described is no longer the
// Repository's locator. It is pure and takes its clock as an argument.
func ShouldObserveRepository(current RepositoryObservation, found bool, locator SourceLocator, now time.Time, staleAfter time.Duration) bool {
	if !found || !current.Recorded() {
		return true
	}
	if current.Locator.Key() != locator.Key() {
		return true
	}
	return current.StaleAt(now, staleAfter)
}

// RepositoryExecutabilityState is the closed set of answers to "can the Loop
// run against this Repository". "unobserved" is a first-class answer: it is
// what must be reported when nothing has been measured, in place of a
// plausible-looking value that was never measured.
type RepositoryExecutabilityState string

const (
	RepositoryExecutable   RepositoryExecutabilityState = "executable"
	RepositoryBlocked      RepositoryExecutabilityState = "blocked"
	RepositoryUnobserved   RepositoryExecutabilityState = "unobserved"
	RepositoryStateRetired RepositoryExecutabilityState = "retired"
)

// RepositoryExecutability answers "ループが実行可能かとその理由" as a value:
// the state, whether it is executable, and the measured reason. Reason is
// always populated, including when Executable is true.
type RepositoryExecutability struct {
	State      RepositoryExecutabilityState `json:"state"`
	Executable bool                         `json:"executable"`
	Reason     string                       `json:"reason"`
	ObservedAt time.Time                    `json:"observed_at,omitempty"`
	Stale      bool                         `json:"stale,omitempty"`
}

// RepositoryExecutabilityFrom derives executability from exactly three
// measured inputs and nothing else: the Repository's own status, the
// effective Control policy for it, and the forge Observation. When no
// Observation exists the answer is "unobserved" with the reason naming the
// missing measurement -- never a guess.
func RepositoryExecutabilityFrom(repository Repository, control EffectiveControlResult, observation RepositoryObservation, found bool, now time.Time, staleAfter time.Duration) RepositoryExecutability {
	if repository.Status == RepositoryRetired {
		return RepositoryExecutability{State: RepositoryStateRetired, Reason: "the Repository is retired; registration was rolled back"}
	}
	mode := control.Mode
	if mode == "" {
		mode = ControlAllow
	}
	if mode != ControlAllow {
		return RepositoryExecutability{State: RepositoryBlocked, Reason: fmt.Sprintf("effective control mode %q denies new work for this Repository", mode)}
	}
	if !found || !observation.Recorded() {
		return RepositoryExecutability{State: RepositoryUnobserved, Reason: "no forge reachability Observation has been submitted for this Repository yet; the Control Plane holds no forge client and never probes the forge itself"}
	}
	stale := observation.StaleAt(now, staleAfter)
	if !observation.Reachable {
		reason := "the forge Observation reports the Repository as unreachable"
		if observation.Reason != "" {
			reason = reason + ": " + observation.Reason
		}
		return RepositoryExecutability{State: RepositoryBlocked, Reason: reason, ObservedAt: observation.ObservedAt, Stale: stale}
	}
	if !observation.CanPush {
		return RepositoryExecutability{State: RepositoryBlocked, Reason: "the forge Observation reports the viewer has no push permission on this Repository", ObservedAt: observation.ObservedAt, Stale: stale}
	}
	if observation.DefaultBranch == "" {
		return RepositoryExecutability{State: RepositoryBlocked, Reason: "the forge Observation carries no default branch", ObservedAt: observation.ObservedAt, Stale: stale}
	}
	return RepositoryExecutability{State: RepositoryExecutable, Executable: true, Reason: "the forge Observation reports the Repository reachable with push permission and a default branch, and no Control Intent denies work for it", ObservedAt: observation.ObservedAt, Stale: stale}
}

// --- V2-071: the Requirement-to-Repository association ---------------------
//
// RequirementRepositoryLink is the write-once side record that says which
// Repository a Requirement belongs to. It is declared here, beside the
// Repository aggregate, and not as a field on Requirement, Increment or
// Repository, for two measured reasons (dp-v2-071 d11, d12).
//
// First, internal/domain/model.go is the proven-closed M1 surface: the
// precedent for adding an association to it is the one already recorded in
// internal/application/ports.go for ControlRequestedByRepository -- the
// immutable aggregate keeps its shape and the new fact is tracked as its own
// keyed record. Second, the association is keyed by the Requirement rather
// than held as a list on the Repository: a slice on Repository would grow
// without bound and would turn every intake into a write to one shared
// document, which is the same contention argument that put
// RepositoryObservation in its own record above.
//
// The record is Requirement-keyed and written at most once. The store, not
// this value object, enforces at-most-once (the domain reads no state), but
// the shape is what makes at-most-once expressible: one Requirement id is the
// whole key, so a second link for the same Requirement is a conflict rather
// than an append.
type RequirementRepositoryLink struct {
	RequirementID RequirementID `json:"requirement_id"`
	RepositoryID  RepositoryID  `json:"repository_id"`
	AssignedAt    time.Time     `json:"assigned_at"`
	// RequestedBy records who caused the association, for attribution only.
	// It is never interpreted, authenticated or authorized here, exactly as
	// Repository.RequestedBy above is not.
	RequestedBy RequestedBy `json:"requested_by,omitempty"`
}

// Recorded distinguishes a real link from a zero value, so a caller can tell
// "this Requirement names no Repository" from "nothing was read".
func (l RequirementRepositoryLink) Recorded() bool {
	return l.RequirementID != "" && l.RepositoryID != ""
}

// ValidateRequirementRepositoryLink is this value object's own invariant
// check, the analogue of ValidateRepository above. It refuses an empty or
// non-opaque identifier on either side and refuses a link that does not say
// when it was made: an association with no instant cannot be ordered against
// the Requirement it describes, so it is not a record of anything.
func ValidateRequirementRepositoryLink(value RequirementRepositoryLink) error {
	if _, err := NewRequirementID(value.RequirementID.String()); err != nil {
		return err
	}
	if _, err := NewRepositoryID(value.RepositoryID.String()); err != nil {
		return err
	}
	if value.AssignedAt.IsZero() {
		return errors.New("requirement-to-repository link requires an explicit assignment timestamp")
	}
	if value.RequestedBy.ActorType != "" && !validActorType(value.RequestedBy.ActorType) {
		return fmt.Errorf("unknown requested_by actor_type %q", value.RequestedBy.ActorType)
	}
	return nil
}

// --- V2-072: publishing one verified commit as a reviewable branch --------
//
// Two additive records live here, beside the Repository aggregate and the
// RepositoryObservation and RequirementRepositoryLink precedents above, for
// the reason those precedents already record (dp-v2-072 d11): a publication
// is keyed by its own identifier rather than held as a growing slice on a
// shared aggregate document, and internal/domain/model.go stays the closed M1
// surface it is. Neither record stores a URL: dp-v2-064 d2 already ruled that
// a URL is never identity and that some URL forms can carry credential
// material, so the human-readable link is derived from the locator, the ref
// and the commit at the moment it is displayed.

// PublicationRefPrefix is the reserved namespace every published ref lives
// under. It is reserved rather than incidental: measured on this repository,
// the only two push-triggered workflows filter branches [v2] and [main]
// respectively (.github/workflows/ci.yml and .github/workflows/deploy.yml),
// so a ref under this prefix matches neither trigger.
const PublicationRefPrefix = "refs/heads/agentic-loop/"

var (
	// ErrInvalidPublicationRef refuses a ref that is not one this Loop
	// produces: outside the reserved prefix, or with a remainder that is not
	// exactly one increment segment and one execution segment.
	ErrInvalidPublicationRef = errors.New("invalid publication ref")
	// ErrInvalidPublicationIntent refuses a publication intent that does not
	// name everything a reviewable branch needs.
	ErrInvalidPublicationIntent = errors.New("invalid publication intent")
	// ErrInvalidPublicationObservation refuses an Observation that does not
	// carry a measured outcome.
	ErrInvalidPublicationObservation = errors.New("invalid publication observation")
)

// validRefSegment reports whether value is one ordinary ref path segment. It
// is deliberately stricter than the forge's own rules: no separator, no
// traversal, no leading dash (which a CLI would read as a flag), no
// whitespace and none of git's own ref metacharacters.
func validRefSegment(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	if strings.ContainsAny(value, "/\\ \t\r\n:?*[~^") || strings.Contains(value, "..") {
		return false
	}
	return !strings.HasPrefix(value, "-")
}

// PublicationRefName is the ONLY producer of a published ref name anywhere in
// this repository (wo-v2-072 A19). It is a pure, total function of the two
// identifiers that own the publication, which is what makes two facts
// structural rather than promised: retrying one Execution's publication
// targets exactly one ref, and a second Execution of the same Increment gets
// its own ref instead of moving the first one.
func PublicationRefName(incrementID IncrementID, executionID ExecutionID) (string, error) {
	increment := strings.TrimSpace(incrementID.String())
	execution := strings.TrimSpace(executionID.String())
	if !validRefSegment(increment) || !validRefSegment(execution) {
		return "", fmt.Errorf("%w: the increment and execution identifiers must each be one ordinary ref segment", ErrInvalidPublicationRef)
	}
	return PublicationRefPrefix + increment + "/" + execution, nil
}

// ParsePublicationRef is the inverse read, not a second producer: it reports
// which Increment and Execution a ref names, and refuses anything the
// producer above could not have emitted. A store projects the increment
// identifier out of a recorded ref with this function, so the Observation
// record itself carries no field the producer does not already determine.
func ParsePublicationRef(ref string) (IncrementID, ExecutionID, error) {
	if !strings.HasPrefix(ref, PublicationRefPrefix) {
		return "", "", fmt.Errorf("%w: %q is not under the reserved prefix", ErrInvalidPublicationRef, ref)
	}
	remainder := strings.TrimPrefix(ref, PublicationRefPrefix)
	increment, execution, found := strings.Cut(remainder, "/")
	if !found || !validRefSegment(increment) || !validRefSegment(execution) {
		return "", "", fmt.Errorf("%w: the remainder must be one increment segment and one execution segment", ErrInvalidPublicationRef)
	}
	return IncrementID(increment), ExecutionID(execution), nil
}

// validObjectName reports whether value looks like a Git object name in
// either of the two widths this Loop sees (SHA-1 and SHA-256). It checks the
// shape only: the domain never computes or verifies a digest.
func validObjectName(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// PublicationIntent is the bounded description of one publication, serialised
// as the Outbox Item's Payload. It carries the coordinate that was resolved
// from a registered Repository inside the command's own transaction, the ref
// PublicationRefName produced, and the four Git facts a content-addressed
// read-after-write compares against. It carries no URL and no credential.
type PublicationIntent struct {
	RepositoryID RepositoryID  `json:"repository_id"`
	Locator      SourceLocator `json:"locator"`
	Ref          string        `json:"ref"`
	BaseBranch   string        `json:"base_branch"`
	BaseCommit   string        `json:"base_commit"`
	HeadCommit   string        `json:"head_commit"`
	HeadTree     string        `json:"head_tree"`
	ChangedPaths int           `json:"changed_paths"`
}

// Recorded distinguishes a real intent from a zero value.
func (i PublicationIntent) Recorded() bool { return i.RepositoryID != "" && i.Ref != "" }

// ValidatePublicationIntent is this value object's own invariant check, the
// analogue of ValidateRepository above.
func ValidatePublicationIntent(value PublicationIntent) error {
	if _, err := NewRepositoryID(value.RepositoryID.String()); err != nil {
		return err
	}
	normalized, err := NormalizeSourceLocator(value.Locator)
	if err != nil {
		return err
	}
	if normalized != value.Locator {
		return fmt.Errorf("%w: locator is not in normalised form", ErrInvalidSourceLocator)
	}
	if _, _, err = ParsePublicationRef(value.Ref); err != nil {
		return err
	}
	if strings.TrimSpace(value.BaseBranch) == "" {
		return fmt.Errorf("%w: the base branch is required", ErrInvalidPublicationIntent)
	}
	if !validObjectName(value.BaseCommit) || !validObjectName(value.HeadCommit) || !validObjectName(value.HeadTree) {
		return fmt.Errorf("%w: the base commit, head commit and head tree must each be an object name", ErrInvalidPublicationIntent)
	}
	if value.BaseCommit == value.HeadCommit {
		return fmt.Errorf("%w: the head commit is the base commit, so there is nothing to publish", ErrInvalidPublicationIntent)
	}
	if value.ChangedPaths <= 0 {
		return fmt.Errorf("%w: a publication with no changed path is not a reviewable change", ErrInvalidPublicationIntent)
	}
	return nil
}

// PublicationState is the closed set of states a publication Observation may
// carry (wo-v2-072 A14). Two properties of this vocabulary are load-bearing
// rather than stylistic.
//
// First, "unobserved" is a first-class value: it is what must be recorded
// when nothing has been measured, in place of a plausible-looking value that
// was never measured, exactly as RepositoryUnobserved above is.
//
// Second, no value in this set means completed, resolved, accepted or done.
// A published branch is not a finished Requirement, and the cheapest durable
// guard against a reader concluding otherwise is that the record's own
// vocabulary cannot say it (dp-v2-072 d16, non_goal 2). The terminal success
// value means published and observed, and nothing more.
type PublicationState string

const (
	// PublicationUnobserved is the state of a publication nothing has
	// measured yet.
	PublicationUnobserved PublicationState = "unobserved"
	// PublicationPublishedAndObserved is the terminal success value: the ref
	// was created by this operation and every content-addressed equality was
	// measured to hold.
	PublicationPublishedAndObserved PublicationState = "published-and-observed"
	// PublicationConvergedOnExistingRef is the idempotent-replay outcome: the
	// ref was already present carrying the intended tree, so this attempt
	// created nothing and the earlier attempt's result stands.
	PublicationConvergedOnExistingRef PublicationState = "converged-on-existing-ref"
	// PublicationRefDisagrees is the undecidable outcome: the ref exists and
	// carries a different tree. Nothing is guessed and nothing is forced; the
	// Outbox Item reaches needs-input.
	PublicationRefDisagrees PublicationState = "ref-disagrees"
	// PublicationRefusedBeforeWrite is the outcome of a publication refused
	// before any forge call could exist.
	PublicationRefusedBeforeWrite PublicationState = "refused-before-write"
)

// PublicationStates returns the closed set, in declaration order. It is the
// one enumeration a caller or a test may read, so "closed" is checkable
// rather than asserted.
func PublicationStates() []PublicationState {
	return []PublicationState{
		PublicationUnobserved,
		PublicationPublishedAndObserved,
		PublicationConvergedOnExistingRef,
		PublicationRefDisagrees,
		PublicationRefusedBeforeWrite,
	}
}

// ValidPublicationState reports membership of the closed set above. An
// unknown value is refused rather than tolerated.
func ValidPublicationState(state PublicationState) bool {
	for _, candidate := range PublicationStates() {
		if candidate == state {
			return true
		}
	}
	return false
}

// PublicationObservation is bounded forge evidence about one publication,
// keyed by the operation identifier and written at most once. It is never
// canonical state and never a command, exactly as RepositoryObservation above
// is not: it carries no raw process output, no response body, no status text
// and no credential.
//
// LocalCommit and PublishedCommit are BOTH recorded and their agreement is
// deliberately NOT required. A commit object is constructed by the forge, so
// a difference in how the author or committer fields are serialised changes
// its object name; requiring equality would assert an unmeasured fact about
// another system's encoding (dp-v2-072 d5, R2). TreesAgree is the equality
// that is required, because a tree object name is a content address computed
// the same way on both sides.
type PublicationObservation struct {
	OperationID     OperationID      `json:"operation_id"`
	RepositoryID    RepositoryID     `json:"repository_id"`
	Ref             string           `json:"ref"`
	PublishedCommit string           `json:"published_commit,omitempty"`
	PublishedTree   string           `json:"published_tree,omitempty"`
	LocalCommit     string           `json:"local_commit,omitempty"`
	LocalTree       string           `json:"local_tree,omitempty"`
	TreesAgree      bool             `json:"trees_agree"`
	State           PublicationState `json:"state"`
	Reason          string           `json:"reason"`
	ObservedAt      time.Time        `json:"observed_at"`
}

// Recorded distinguishes a real Observation from a zero value.
func (o PublicationObservation) Recorded() bool {
	return o.OperationID != "" && !o.ObservedAt.IsZero()
}

// IncrementID reports which Increment this Observation's ref names. It is a
// read of the ref through ParsePublicationRef, not a stored field, so the ref
// stays the single source of that association.
func (o PublicationObservation) IncrementID() (IncrementID, error) {
	increment, _, err := ParsePublicationRef(o.Ref)
	return increment, err
}

// ValidatePublicationObservation is this value object's own invariant check.
// A publication that measured nothing must say so with the unobserved state
// rather than with empty success fields, and a terminal success must actually
// carry the two published object names and an agreeing tree.
func ValidatePublicationObservation(value PublicationObservation) error {
	if _, err := NewOperationID(value.OperationID.String()); err != nil {
		return err
	}
	if _, err := NewRepositoryID(value.RepositoryID.String()); err != nil {
		return err
	}
	if _, _, err := ParsePublicationRef(value.Ref); err != nil {
		return err
	}
	if !ValidPublicationState(value.State) {
		return fmt.Errorf("%w: unknown state %q", ErrInvalidPublicationObservation, value.State)
	}
	if strings.TrimSpace(value.Reason) == "" {
		return fmt.Errorf("%w: a measured outcome states its reason", ErrInvalidPublicationObservation)
	}
	if value.ObservedAt.IsZero() {
		return fmt.Errorf("%w: an Observation with no instant is not a record of anything", ErrInvalidPublicationObservation)
	}
	for name, object := range map[string]string{
		"published_commit": value.PublishedCommit,
		"published_tree":   value.PublishedTree,
		"local_commit":     value.LocalCommit,
		"local_tree":       value.LocalTree,
	} {
		if object != "" && !validObjectName(object) {
			return fmt.Errorf("%w: %s is not an object name", ErrInvalidPublicationObservation, name)
		}
	}
	switch value.State {
	case PublicationPublishedAndObserved, PublicationConvergedOnExistingRef:
		if value.PublishedCommit == "" || value.PublishedTree == "" || value.LocalCommit == "" || value.LocalTree == "" {
			return fmt.Errorf("%w: a successful publication records both the published and the local object names", ErrInvalidPublicationObservation)
		}
		if !value.TreesAgree || value.PublishedTree != value.LocalTree {
			return fmt.Errorf("%w: a successful publication requires the published tree to equal the locally verified tree", ErrInvalidPublicationObservation)
		}
	case PublicationUnobserved, PublicationRefusedBeforeWrite:
		if value.PublishedCommit != "" || value.PublishedTree != "" {
			return fmt.Errorf("%w: state %q may carry no published object name", ErrInvalidPublicationObservation, value.State)
		}
	}
	return nil
}
