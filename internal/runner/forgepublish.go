package runner

// The first production EffectSink in this repository (V2-072). It turns one
// verified local commit into one branch a person can open on the real forge,
// and then reads that branch back.
//
// It is a NEW type, deliberately not a method on ForgeClient:
// internal/runner/forge.go states in its own header that the adapter is
// read-only and has no code path that creates, modifies or deletes anything
// on the forge, and that statement stays true because this file is where every
// write lives. forge.go is byte-identical.
//
// Six properties are structural rather than promised:
//
//  1. The write is the forge CLI's Git Data API as a child process, never
//     `git push` (dp-v2-072 d2). No credential enters argv, the environment or
//     any file: the CLI reads its own store, the granted set is empty, and
//     every request body travels on the child's standard input.
//  2. The set of API operations this adapter can NAME is a closed list read
//     from exactly one place, so an argv or a body naming a ref update, a ref
//     delete or a force is not constructible at all. That is the same
//     mechanism V2-071 used for `git push`: the list itself is the refusal.
//  3. Confirmation is four content-addressed equalities, never a status code
//     and never an error string. The adapter parses no HTTP status, discards
//     the child's standard error outright, bounds its standard output with a
//     hard byte cap, and returns sentinels carrying none of the child's bytes.
//  4. What is published comes from the VERIFIED commit through
//     PublicationSource, not from the caller's ChangeSet.
//  5. Immediately before its first write it re-reads the publication target
//     through the application layer and refuses a payload that disagrees:
//     docs/architecture/domain-model.md section 10 requires that the next
//     Runner not trust the Work Packet and re-observe external state, and the
//     publication payload is the one packet that would otherwise be trusted
//     blindly.
//  6. An outcome it cannot decide is reported as exactly that, through
//     application.ErrEffectUndecidable, so the Outbox Item reaches needs-input
//     instead of a guess. It never forces and never updates a ref.
//
// This adapter is NOT filesystem-confined, and that is stated rather than
// hidden (dp-v2-072 d3): it writes nothing into the Execution workspace, so a
// mount namespace would bound nothing it does. It is bounded instead by a
// closed argv shape, an empty granted set, a bounded output cap and a context
// deadline. NamespaceConfinement is not relaxed by one bit and no git child
// changes at all.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// ForgeAPIOperation names one operation this adapter may perform. The type is
// closed by forgePublishOperations below.
type ForgeAPIOperation string

const (
	// The four writes.
	ForgeCreateBlob   ForgeAPIOperation = "create-blob"
	ForgeCreateTree   ForgeAPIOperation = "create-tree"
	ForgeCreateCommit ForgeAPIOperation = "create-commit"
	ForgeCreateRef    ForgeAPIOperation = "create-ref"
	// The two reads that decide the outcome.
	ForgeReadRef    ForgeAPIOperation = "read-ref"
	ForgeReadCommit ForgeAPIOperation = "read-commit"
	// The one measurement read: how many workflow runs exist for a branch.
	ForgeCountWorkflowRuns ForgeAPIOperation = "count-workflow-runs"
)

// forgeAPISpec is one entry of the closed list. suffix is appended to
// "repos/<owner>/<name>/" and may carry exactly one %s for the operation's
// single argument.
type forgeAPISpec struct {
	method string
	suffix string
	writes bool
	body   bool
}

// forgePublishOperations is the CLOSED set of API operations this adapter can
// name, and it is the single point of refusal: forgeOperationSpec is its only
// reader and every argv passes through it, so an operation outside this list
// cannot be expressed. Note by construction what is absent -- there is no
// entry that updates a ref, deletes a ref, forces anything, creates a Pull
// Request, a tag, a release, an issue or a comment -- and that absence is the
// mechanism rather than a comment.
var forgePublishOperations = map[ForgeAPIOperation]forgeAPISpec{
	ForgeCreateBlob:        {method: "POST", suffix: "git/blobs", writes: true, body: true},
	ForgeCreateTree:        {method: "POST", suffix: "git/trees", writes: true, body: true},
	ForgeCreateCommit:      {method: "POST", suffix: "git/commits", writes: true, body: true},
	ForgeCreateRef:         {method: "POST", suffix: "git/refs", writes: true, body: true},
	ForgeReadRef:           {method: "GET", suffix: "git/matching-refs/%s"},
	ForgeReadCommit:        {method: "GET", suffix: "git/commits/%s"},
	ForgeCountWorkflowRuns: {method: "GET", suffix: "actions/runs?per_page=1&branch=%s"},
}

// ForgePublishOperations returns the closed list, sorted, so a test can assert
// membership and absence without reaching into the map.
func ForgePublishOperations() []ForgeAPIOperation {
	return []ForgeAPIOperation{
		ForgeCreateBlob, ForgeCreateTree, ForgeCreateCommit, ForgeCreateRef,
		ForgeReadRef, ForgeReadCommit, ForgeCountWorkflowRuns,
	}
}

// forgeOperationSpec is the closed list's ONLY reader.
func forgeOperationSpec(operation ForgeAPIOperation) (forgeAPISpec, error) {
	spec, ok := forgePublishOperations[operation]
	if !ok {
		return forgeAPISpec{}, fmt.Errorf("%w: %q", ErrForgeOperationNotAllowed, operation)
	}
	return spec, nil
}

var (
	// ErrForgeOperationNotAllowed refuses an operation outside the closed
	// list. It is what makes "no ref update and no force is constructible" a
	// property of the code rather than a promise.
	ErrForgeOperationNotAllowed = errors.New("forgepublish: the API operation is not in the closed allowlist")
	// ErrForgeRefNotReserved refuses a ref that is not under the reserved
	// prefix this Loop publishes into.
	ErrForgeRefNotReserved = errors.New("forgepublish: the ref is not under the reserved publication prefix")
	// ErrForgeTargetDisagrees refuses a payload that disagrees with the
	// publication target read back from the application layer.
	ErrForgeTargetDisagrees = errors.New("forgepublish: the payload disagrees with the stored publication target")
	// ErrForgePayloadDisagrees refuses a locally derived payload that
	// disagrees with the intent it is supposed to publish.
	ErrForgePayloadDisagrees = errors.New("forgepublish: the locally derived payload disagrees with the publication intent")
	// ErrForgeBlobDisagrees, ErrForgeTreeDisagrees, ErrForgeCommitDisagrees
	// and ErrForgeRefDisagrees are the four content-addressed equalities,
	// each with its own sentinel so a failure names which equality failed
	// without quoting any response.
	ErrForgeBlobDisagrees   = errors.New("forgepublish: a created blob object name does not equal the local one")
	ErrForgeTreeDisagrees   = errors.New("forgepublish: the created tree object name does not equal the locally verified head tree")
	ErrForgeCommitDisagrees = errors.New("forgepublish: the published commit does not carry the intended tree and exactly the base commit as its parent")
	ErrForgeRefDisagrees    = errors.New("forgepublish: the ref read back is not the ref this operation intended")
	// ErrForgePublishUnreadable reports that a response could not be projected
	// onto the bounded shape this adapter reads. Its message deliberately
	// carries none of that response.
	ErrForgePublishUnreadable = errors.New("forgepublish: the CLI response could not be parsed into the bounded observation")
	// ErrForgePublishIncomplete reports that a call did not complete. Its
	// message carries no process output either.
	ErrForgePublishIncomplete = errors.New("forgepublish: the API call did not complete")
	// ErrForgePublishNotConfigured refuses a publisher that is missing one of
	// its collaborators, before any process could exist.
	ErrForgePublishNotConfigured = errors.New("forgepublish: the publisher is missing a required collaborator")
)

// maxForgePublishResponseBytes bounds how much of a child's standard output
// this adapter is willing to hold. A Git Data API response is a few kilobytes;
// the cap exists so a pathological response cannot become unbounded memory.
const maxForgePublishResponseBytes = 1 << 20

// DefaultForgePublishDeadline bounds one child's lifetime. It is applied as a
// context deadline, never as a timer and never as a sleep.
const DefaultForgePublishDeadline = 60 * time.Second

// PublicationTargetReader is the application-layer read A18 requires. It is
// declared here, narrow, so the adapter depends on one method rather than on
// the whole Service.
type PublicationTargetReader interface {
	PublicationTargetForOutbox(ctx context.Context, outboxID string) (application.PublicationTarget, bool, error)
}

// PublicationRecorder is the application-layer write-once Observation surface:
// the write, and the read that lets this adapter RESPECT write-once rather than
// fight it. The first measured outcome for one operation identifier stands, so
// a later observation of the same operation reads the existing record and adds
// nothing instead of producing a conflict.
type PublicationRecorder interface {
	RecordPublication(ctx context.Context, value domain.PublicationObservation) error
	Publication(ctx context.Context, operationID string) (domain.PublicationObservation, bool, error)
}

// ForgeCall is one bounded external call: its operation, its complete argv and
// its request body. It exists so the transport can be injected as a FUNCTION
// in the deterministic tests -- not as a fake dispatcher and not as a fake
// store -- while the protocol under test stays the real one.
type ForgeCall struct {
	Operation ForgeAPIOperation
	Argv      []string
	Body      []byte
}

// ForgeTransport performs one ForgeCall and returns its bounded standard
// output. A nil transport means the real child process.
type ForgeTransport func(ctx context.Context, call ForgeCall) ([]byte, error)

// ForgePublisher publishes one verified commit as one reviewable branch, and
// observes the result. It holds no credential of any kind and consults no
// Secret Broker: the CLI reads its own store, which is what makes the absence
// provable.
type ForgePublisher struct {
	// ExecutablePath, when empty, is resolved with the same resolveTool helper
	// forge.go and git.go use. The fallback is load-bearing rather than
	// defensive: it is measured that the validated `devbox run --pure`
	// environment has git and unshare on PATH but not the forge CLI.
	ExecutablePath string
	// GrantSet must stay empty. A non-empty one is refused before any process
	// could exist.
	GrantSet []string
	// BaseEnvironmentNames names the variables that cross into the child.
	BaseEnvironmentNames []string
	AdapterVersion       string
	// Stat, when non-nil, replaces os.Stat, so every executable refusal is
	// deterministic and touches no filesystem.
	Stat func(string) (os.FileInfo, error)
	// Source derives what is published from the verified commit.
	Source PublicationSource
	// Working is the working copy that verified commit lives in.
	Working WorkingCopy
	// Target and Records are the two application-layer collaborators.
	Target  PublicationTargetReader
	Records PublicationRecorder
	// Transport, when non-nil, replaces the real child process. It is the ONLY
	// injection point in this adapter.
	Transport ForgeTransport
	// Now is the injected clock. A nil Now means time.Now().UTC().
	Now func() time.Time
	// Deadline bounds one child's lifetime as a context deadline.
	Deadline time.Duration
}

// NewForgePublisher returns a publisher with the measured defaults.
func NewForgePublisher(adapterVersion string) ForgePublisher {
	return ForgePublisher{
		GrantSet:             nil,
		BaseEnvironmentNames: append([]string(nil), DefaultForgeBaseEnvironmentNames...),
		AdapterVersion:       adapterVersion,
		Deadline:             DefaultForgePublishDeadline,
	}
}

func (p ForgePublisher) stat(path string) (os.FileInfo, error) {
	if p.Stat != nil {
		return p.Stat(path)
	}
	return os.Stat(path)
}

func (p ForgePublisher) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

// ResolveExecutable resolves and validates the CLI path without starting
// anything. Every refusal here happens before a process could exist, and it is
// the same idiom forge.go uses, re-implemented rather than shared so that
// forge.go stays byte-identical.
func (p ForgePublisher) ResolveExecutable() (string, error) {
	if len(p.GrantSet) != 0 {
		return "", fmt.Errorf("%w: %d granted name(s) supplied", ErrForgeGrantSetNotEmpty, len(p.GrantSet))
	}
	candidate := strings.TrimSpace(p.ExecutablePath)
	if candidate == "" {
		candidate = resolveTool(forgeExecutableName)
	}
	if !filepath.IsAbs(candidate) {
		return "", fmt.Errorf("%w: %q is not an absolute path", ErrForgeExecutableMissing, candidate)
	}
	if filepath.Base(candidate) != forgeExecutableName {
		return "", fmt.Errorf("%w: basename(%s) != %s", ErrForgeExecutableMismatch, candidate, forgeExecutableName)
	}
	info, err := p.stat(candidate)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrForgeExecutableMissing, candidate)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%w: %s is a directory", ErrForgeExecutableMissing, candidate)
	}
	return candidate, nil
}

// ChildEnvironment builds the guarded base environment the child receives: the
// home directory, so the CLI can find its own configuration store without this
// code naming it, and PATH. Nothing else crosses, and the granted channel is
// empty by construction, so there is nothing for the Secret Broker to hand
// over and nothing for a grant to leak.
func (p ForgePublisher) ChildEnvironment() ([]string, error) {
	if len(p.GrantSet) != 0 {
		return nil, fmt.Errorf("%w: %d granted name(s) supplied", ErrForgeGrantSetNotEmpty, len(p.GrantSet))
	}
	names := p.BaseEnvironmentNames
	if len(names) == 0 {
		names = DefaultForgeBaseEnvironmentNames
	}
	env, err := buildEnvironmentFromBaseNames(names)
	if err != nil {
		return nil, fmt.Errorf("forgepublish: %w", err)
	}
	if err = GuardEnvironment(env, allowlistFromNames(names)); err != nil {
		return nil, fmt.Errorf("forgepublish: %w", err)
	}
	return env, nil
}

// Argv is the complete argv of one operation, built without starting anything
// so it can be asserted directly. The method is stated explicitly rather than
// relied on as a default, so the argv itself witnesses what the call does, and
// the repository path is built from validated single-segment owner and name so
// a coordinate can never inject an extra API path.
//
// argument is the operation's single path argument (a ref name, a commit
// object name or a branch name) and must be absent for an operation whose
// suffix carries no placeholder.
func (p ForgePublisher) Argv(operation ForgeAPIOperation, owner, name, argument string) ([]string, error) {
	spec, err := forgeOperationSpec(operation)
	if err != nil {
		return nil, err
	}
	resolved, err := p.ResolveExecutable()
	if err != nil {
		return nil, err
	}
	if !validForgeSegment(owner) || !validForgeSegment(name) {
		return nil, fmt.Errorf("%w: owner=%q name=%q", ErrForgeCoordinateInvalid, owner, name)
	}
	suffix := spec.suffix
	if strings.Contains(suffix, "%s") {
		if argument == "" || strings.HasPrefix(argument, "-") || strings.ContainsAny(argument, " \t\r\n\\") {
			return nil, fmt.Errorf("%w: operation %q requires one path argument", ErrForgeOperationNotAllowed, operation)
		}
		suffix = strings.Replace(suffix, "%s", argument, 1)
	} else if argument != "" {
		return nil, fmt.Errorf("%w: operation %q takes no path argument", ErrForgeOperationNotAllowed, operation)
	}
	argv := []string{resolved, "api", "--method", spec.method, forgeAPIPathPrefix + owner + "/" + name + "/" + suffix}
	if spec.body {
		// The body travels on standard input, never in argv. "--input -" is
		// what makes that explicit in the argv itself.
		argv = append(argv, "--input", "-")
	}
	return argv, nil
}

// VersionArgv is the argv that reports the CLI's own version, recorded as an
// execution-environment identifier. It reads nothing from the forge and writes
// nothing.
func (p ForgePublisher) VersionArgv() ([]string, error) {
	resolved, err := p.ResolveExecutable()
	if err != nil {
		return nil, err
	}
	return []string{resolved, "--version"}, nil
}

// --- request bodies --------------------------------------------------------
//
// Every body is a typed struct with explicit json tags, so the set of keys a
// body can carry is the set of fields declared here. None of them is named
// "force", and there is no map-shaped body anywhere in this file, so no key
// can be added at run time.

type forgeBlobRequest struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

type forgeTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

type forgeTreeRequest struct {
	BaseTree string           `json:"base_tree"`
	Tree     []forgeTreeEntry `json:"tree"`
}

type forgeCommitRequest struct {
	Message string   `json:"message"`
	Tree    string   `json:"tree"`
	Parents []string `json:"parents"`
}

type forgeRefRequest struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// BlobBody, TreeBody, CommitBody and RefBody are the four write bodies, built
// as pure functions so every field can be asserted without a process.
func BlobBody(file PublicationFile) ([]byte, error) {
	if file.Content == "" {
		return nil, fmt.Errorf("%w: a blob body with no content", ErrForgePayloadDisagrees)
	}
	return json.Marshal(forgeBlobRequest{Content: file.Content, Encoding: "base64"})
}

func TreeBody(payload PublicationPayload) ([]byte, error) {
	if payload.BaseTree == "" || len(payload.Files) == 0 {
		return nil, fmt.Errorf("%w: a tree body needs a base tree and at least one entry", ErrForgePayloadDisagrees)
	}
	entries := make([]forgeTreeEntry, 0, len(payload.Files))
	for _, file := range payload.Files {
		if file.Mode != publicationModeFile && file.Mode != publicationModeExecutable {
			return nil, fmt.Errorf("%w: mode %s", ErrPublicationModeUnrepresentable, file.Mode)
		}
		entries = append(entries, forgeTreeEntry{Path: file.Path, Mode: file.Mode, Type: "blob", SHA: file.Object})
	}
	return json.Marshal(forgeTreeRequest{BaseTree: payload.BaseTree, Tree: entries})
}

func CommitBody(message, tree, baseCommit string) ([]byte, error) {
	if message == "" || tree == "" || baseCommit == "" {
		return nil, fmt.Errorf("%w: a commit body needs a message, a tree and one parent", ErrForgePayloadDisagrees)
	}
	return json.Marshal(forgeCommitRequest{Message: message, Tree: tree, Parents: []string{baseCommit}})
}

func RefBody(ref, commit string) ([]byte, error) {
	if _, _, err := domain.ParsePublicationRef(ref); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrForgeRefNotReserved, err)
	}
	if commit == "" {
		return nil, fmt.Errorf("%w: a ref body needs a commit", ErrForgePayloadDisagrees)
	}
	return json.Marshal(forgeRefRequest{Ref: ref, SHA: commit})
}

// PublicationCommitMessage is the message the published commit carries. It is
// a deterministic function of the two identifiers that own the publication and
// carries nothing else: no prompt, no provider output, no path and nothing
// credential-shaped.
func PublicationCommitMessage(ref string) (string, error) {
	increment, execution, err := domain.ParsePublicationRef(ref)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrForgeRefNotReserved, err)
	}
	return "publish the verified change of increment " + increment.String() + ", execution " + execution.String(), nil
}

// --- bounded response shapes ----------------------------------------------
//
// Each struct is the bound: every other field of a response is discarded
// unparsed, and no raw response ever leaves this file.

type forgeObjectResponse struct {
	SHA string `json:"sha"`
}

type forgeRefResponse struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

type forgeCommitResponse struct {
	SHA  string `json:"sha"`
	Tree struct {
		SHA string `json:"sha"`
	} `json:"tree"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

type forgeWorkflowRunsResponse struct {
	TotalCount int `json:"total_count"`
}

func decodeForgeResponse(raw []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(out); err != nil {
		// The input is deliberately not wrapped: a forge error body can carry
		// a repository name, a scope list or a rate-limit header.
		return ErrForgePublishUnreadable
	}
	return nil
}

// --- the transport ---------------------------------------------------------

// call performs one operation. It is the only place a process can start.
func (p ForgePublisher) call(ctx context.Context, operation ForgeAPIOperation, owner, name, argument string, body []byte) ([]byte, error) {
	spec, err := forgeOperationSpec(operation)
	if err != nil {
		return nil, err
	}
	if spec.body != (len(body) > 0) {
		return nil, fmt.Errorf("%w: operation %q body presence", ErrForgeOperationNotAllowed, operation)
	}
	argv, err := p.Argv(operation, owner, name, argument)
	if err != nil {
		return nil, err
	}
	call := ForgeCall{Operation: operation, Argv: argv, Body: append([]byte(nil), body...)}
	if p.Transport != nil {
		// The injected transport is held to the same rule as the real child:
		// nothing it returns may carry bytes out of this adapter. Its error is
		// therefore reduced to this adapter's own sentinel unless it already is
		// one of the two the protocol distinguishes.
		raw, err := p.Transport(ctx, call)
		if err != nil {
			if errors.Is(err, ErrForgePublishIncomplete) || errors.Is(err, application.ErrEffectUndecidable) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %s", ErrForgePublishIncomplete, call.Operation)
		}
		return raw, nil
	}
	env, err := p.ChildEnvironment()
	if err != nil {
		return nil, err
	}
	if err = GuardCommand(argv, env); err != nil {
		return nil, fmt.Errorf("forgepublish: %w", err)
	}
	deadline := p.Deadline
	if deadline <= 0 {
		deadline = DefaultForgePublishDeadline
	}
	bounded, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	return runForgePublishProcess(bounded, call, env)
}

// runForgePublishProcess starts exactly one child with the already-resolved
// absolute argv and the already-guarded environment, writes the request body to
// its standard input, captures its standard output into a bounded buffer, and
// returns either those bytes or an error. Standard error is discarded rather
// than captured, so there is no path by which a forge error body could reach a
// caller, a log or a record.
func runForgePublishProcess(ctx context.Context, call ForgeCall, env []string) ([]byte, error) {
	argv := call.Argv
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) || filepath.Base(argv[0]) != forgeExecutableName {
		return nil, ErrForgeExecutableMismatch
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append([]string(nil), env...)
	if len(call.Body) > 0 {
		cmd.Stdin = bytes.NewReader(call.Body)
	} else {
		cmd.Stdin = nil
	}
	cmd.Stderr = nil
	var stdout bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, remaining: maxForgePublishResponseBytes}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrForgePublishIncomplete, call.Operation)
	}
	return stdout.Bytes(), nil
}

// --- the reads that decide -------------------------------------------------

// refState is the three-valued answer a ref read can give. "unknown" is a
// first-class value on purpose: without parsing a status code the adapter
// cannot distinguish "this ref does not exist" from "the read did not
// complete", and pretending otherwise would be a fabricated observation.
//
// Proceeding to the create on "unknown" is safe rather than optimistic: ref
// creation is a compare-and-set against absence, so a create issued while the
// ref actually exists fails, and the post-failure read-after-write is what
// decides. Nothing is ever forced and no ref is ever updated.
type refState int

const (
	refAbsent refState = iota
	refPresent
	refUnknown
)

// readRef reads the ref through the forge's MATCHING-REFS form rather than the
// single-ref form, and that choice is what makes absence measurable: the
// matching form answers with an empty list when nothing matches, so "this ref
// does not exist" is a successful read rather than an error this adapter would
// have to distinguish from a transport failure by parsing a status code, which
// it refuses to do. The match is a prefix match on the forge's side, so an
// exact equality on the ref field is required here; a list that contains only
// longer names means this ref is absent.
func (p ForgePublisher) readRef(ctx context.Context, owner, name, ref string) (forgeRefResponse, refState, error) {
	if _, _, err := domain.ParsePublicationRef(ref); err != nil {
		return forgeRefResponse{}, refUnknown, fmt.Errorf("%w: %v", ErrForgeRefNotReserved, err)
	}
	raw, err := p.call(ctx, ForgeReadRef, owner, name, strings.TrimPrefix(ref, "refs/"), nil)
	if err != nil {
		if errors.Is(err, ErrForgePublishIncomplete) {
			return forgeRefResponse{}, refUnknown, nil
		}
		return forgeRefResponse{}, refUnknown, err
	}
	var parsed []forgeRefResponse
	if err = decodeForgeResponse(raw, &parsed); err != nil {
		return forgeRefResponse{}, refUnknown, err
	}
	for _, candidate := range parsed {
		if candidate.Ref != ref {
			continue
		}
		if candidate.Object.SHA == "" || candidate.Object.Type != "commit" {
			return forgeRefResponse{}, refUnknown, ErrForgePublishUnreadable
		}
		return candidate, refPresent, nil
	}
	return forgeRefResponse{}, refAbsent, nil
}

func (p ForgePublisher) readCommit(ctx context.Context, owner, name, commit string) (forgeCommitResponse, error) {
	raw, err := p.call(ctx, ForgeReadCommit, owner, name, commit, nil)
	if err != nil {
		return forgeCommitResponse{}, err
	}
	var parsed forgeCommitResponse
	if err = decodeForgeResponse(raw, &parsed); err != nil {
		return forgeCommitResponse{}, err
	}
	if parsed.SHA == "" || parsed.Tree.SHA == "" {
		return forgeCommitResponse{}, ErrForgePublishUnreadable
	}
	return parsed, nil
}

// WorkflowRunCount is the one measurement read: how many workflow runs the
// forge recorded for one branch. It is used by the gated live check to turn
// "no CI is triggered" from an argument about YAML into an observation.
func (p ForgePublisher) WorkflowRunCount(ctx context.Context, owner, name, branch string) (int, error) {
	raw, err := p.call(ctx, ForgeCountWorkflowRuns, owner, name, branch, nil)
	if err != nil {
		return 0, err
	}
	var parsed forgeWorkflowRunsResponse
	if err = decodeForgeResponse(raw, &parsed); err != nil {
		return 0, err
	}
	return parsed.TotalCount, nil
}

// --- the sink and the observer ---------------------------------------------

func (p ForgePublisher) configured() error {
	if p.Source == nil || p.Target == nil || p.Records == nil {
		return fmt.Errorf("%w: a source, a target reader and a recorder are all required", ErrForgePublishNotConfigured)
	}
	return nil
}

// publicationIntent projects one delivery's payload onto the bounded intent.
func publicationIntent(delivery application.EffectDelivery) (domain.PublicationIntent, error) {
	var intent domain.PublicationIntent
	if err := json.Unmarshal(delivery.Payload, &intent); err != nil {
		return intent, fmt.Errorf("%w: the delivery payload is not a publication intent", application.ErrInvalidOutbox)
	}
	if err := domain.ValidatePublicationIntent(intent); err != nil {
		return intent, err
	}
	return intent, nil
}

// record writes the write-once Observation. A failure to record is returned:
// an external effect whose Observation could not be stored is not a success.
func (p ForgePublisher) record(ctx context.Context, delivery application.EffectDelivery, intent domain.PublicationIntent, state domain.PublicationState, publishedCommit, publishedTree, reason string, treesAgree bool) error {
	// Write-once is respected rather than fought: if this operation already
	// carries an Observation, that first measured outcome stands and nothing
	// is written. Overwriting it would be exactly the "re-observe into a
	// different answer" the record's own write-once rule forbids.
	if _, found, err := p.Records.Publication(ctx, delivery.OperationID); err != nil {
		return err
	} else if found {
		return nil
	}
	observation := domain.PublicationObservation{
		OperationID:     domain.OperationID(delivery.OperationID),
		RepositoryID:    intent.RepositoryID,
		Ref:             intent.Ref,
		PublishedCommit: publishedCommit,
		PublishedTree:   publishedTree,
		LocalCommit:     intent.HeadCommit,
		LocalTree:       intent.HeadTree,
		TreesAgree:      treesAgree,
		State:           state,
		Reason:          reason,
		ObservedAt:      p.now(),
	}
	return p.Records.RecordPublication(ctx, observation)
}

// Deliver publishes one verified commit as one reviewable branch. It is the
// EffectSink half of the protocol and runs outside every transaction.
func (p ForgePublisher) Deliver(ctx context.Context, delivery application.EffectDelivery) error {
	if err := p.configured(); err != nil {
		return err
	}
	intent, err := publicationIntent(delivery)
	if err != nil {
		return err
	}
	owner, name := intent.Locator.Owner, intent.Locator.Name

	// A18: re-read the target through the application layer BEFORE the first
	// write, and refuse a payload that disagrees. No process has started yet.
	target, found, err := p.Target.PublicationTargetForOutbox(ctx, delivery.OutboxID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: no publication target is stored for this delivery", ErrForgeTargetDisagrees)
	}
	if !target.Agrees(intent) {
		return fmt.Errorf("%w: repository, owner, name, ref or base commit", ErrForgeTargetDisagrees)
	}

	// The payload comes from the verified commit, and it must describe the
	// same commit the intent names.
	payload, err := p.Source.PublicationPayload(ctx, p.Working, intent.BaseCommit, intent.HeadCommit)
	if err != nil {
		return err
	}
	if payload.HeadTree != intent.HeadTree {
		return fmt.Errorf("%w: the derived head tree is not the intent's head tree", ErrForgePayloadDisagrees)
	}
	if len(payload.Files) != intent.ChangedPaths {
		return fmt.Errorf("%w: the derived path count is %d and the intent says %d", ErrForgePayloadDisagrees, len(payload.Files), intent.ChangedPaths)
	}

	// Step 2 of the protocol: read the ref before creating it. This is the
	// duplicate detection, and it is a read rather than an interpretation of a
	// previous error.
	existing, state, err := p.readRef(ctx, owner, name, intent.Ref)
	if err != nil {
		return err
	}
	if state == refPresent {
		commit, e := p.readCommit(ctx, owner, name, existing.Object.SHA)
		if e != nil {
			return e
		}
		if commit.Tree.SHA == intent.HeadTree {
			// Converged: the ref already carries the intended tree, so this
			// attempt creates nothing and the earlier attempt's result stands.
			return p.record(ctx, delivery, intent, domain.PublicationConvergedOnExistingRef, commit.SHA, commit.Tree.SHA,
				"the ref was already present carrying the intended tree, so this attempt created nothing", true)
		}
		if e = p.record(ctx, delivery, intent, domain.PublicationRefDisagrees, "", "",
			"the ref exists and its commit carries a different tree; nothing is forced and nothing is guessed", false); e != nil {
			return e
		}
		return fmt.Errorf("%w: the ref exists and carries a different tree", application.ErrEffectUndecidable)
	}

	// The four writes, in order. Each is content-addressed, so each is checked
	// against what was measured locally before the next one is issued.
	for _, file := range payload.Files {
		body, e := BlobBody(file)
		if e != nil {
			return e
		}
		raw, e := p.call(ctx, ForgeCreateBlob, owner, name, "", body)
		if e != nil {
			return e
		}
		var created forgeObjectResponse
		if e = decodeForgeResponse(raw, &created); e != nil {
			return e
		}
		if created.SHA != file.Object {
			return fmt.Errorf("%w: one path", ErrForgeBlobDisagrees)
		}
	}
	treeBody, err := TreeBody(payload)
	if err != nil {
		return err
	}
	raw, err := p.call(ctx, ForgeCreateTree, owner, name, "", treeBody)
	if err != nil {
		return err
	}
	var createdTree forgeObjectResponse
	if err = decodeForgeResponse(raw, &createdTree); err != nil {
		return err
	}
	if createdTree.SHA != intent.HeadTree {
		return fmt.Errorf("%w: the created tree", ErrForgeTreeDisagrees)
	}
	message, err := PublicationCommitMessage(intent.Ref)
	if err != nil {
		return err
	}
	commitBody, err := CommitBody(message, createdTree.SHA, intent.BaseCommit)
	if err != nil {
		return err
	}
	if raw, err = p.call(ctx, ForgeCreateCommit, owner, name, "", commitBody); err != nil {
		return err
	}
	var createdCommit forgeObjectResponse
	if err = decodeForgeResponse(raw, &createdCommit); err != nil {
		return err
	}
	if createdCommit.SHA == "" {
		return ErrForgePublishUnreadable
	}
	refBody, err := RefBody(intent.Ref, createdCommit.SHA)
	if err != nil {
		return err
	}
	if _, err = p.call(ctx, ForgeCreateRef, owner, name, "", refBody); err != nil {
		// The create did not complete. Read the ref again: that read, not the
		// error, decides.
		return p.resolveAfterFailure(ctx, delivery, intent, owner, name)
	}

	// Read-after-write: the ref and its commit, measured.
	return p.confirm(ctx, delivery, intent, owner, name, createdCommit.SHA, domain.PublicationPublishedAndObserved,
		"the ref was created by this operation and all four content-addressed equalities held")
}

// confirm is the read-after-write. It requires all four equalities: the ref
// read back is the ref this operation intended, its object is the commit that
// was created, that commit carries the intended tree, and its parent list is
// exactly the one base commit. The blob equality was already required, one
// object at a time, before the tree was created.
func (p ForgePublisher) confirm(ctx context.Context, delivery application.EffectDelivery, intent domain.PublicationIntent, owner, name, expectCommit string, state domain.PublicationState, reason string) error {
	readBack, refState, err := p.readRef(ctx, owner, name, intent.Ref)
	if err != nil {
		return err
	}
	if refState != refPresent {
		return fmt.Errorf("%w: the ref could not be read back after the write", application.ErrEffectUndecidable)
	}
	if readBack.Ref != intent.Ref {
		return fmt.Errorf("%w: read back %q", ErrForgeRefDisagrees, readBack.Ref)
	}
	if expectCommit != "" && readBack.Object.SHA != expectCommit {
		return fmt.Errorf("%w: the ref does not point at the commit this operation created", ErrForgeCommitDisagrees)
	}
	commit, err := p.readCommit(ctx, owner, name, readBack.Object.SHA)
	if err != nil {
		return err
	}
	if commit.Tree.SHA != intent.HeadTree {
		return fmt.Errorf("%w: the published commit's tree", ErrForgeCommitDisagrees)
	}
	if len(commit.Parents) != 1 || commit.Parents[0].SHA != intent.BaseCommit {
		return fmt.Errorf("%w: the published commit's parent list", ErrForgeCommitDisagrees)
	}
	return p.record(ctx, delivery, intent, state, commit.SHA, commit.Tree.SHA, reason, true)
}

// resolveAfterFailure is the read performed after any failed write. Reads
// decide; the failure's own text never does.
func (p ForgePublisher) resolveAfterFailure(ctx context.Context, delivery application.EffectDelivery, intent domain.PublicationIntent, owner, name string) error {
	readBack, state, err := p.readRef(ctx, owner, name, intent.Ref)
	if err != nil {
		return err
	}
	switch state {
	case refPresent:
		commit, e := p.readCommit(ctx, owner, name, readBack.Object.SHA)
		if e != nil {
			return e
		}
		if commit.Tree.SHA == intent.HeadTree && len(commit.Parents) == 1 && commit.Parents[0].SHA == intent.BaseCommit {
			return p.record(ctx, delivery, intent, domain.PublicationPublishedAndObserved, commit.SHA, commit.Tree.SHA,
				"the ref creation reported no success, and the read afterwards found the intended ref with the intended tree and parent", true)
		}
		if e = p.record(ctx, delivery, intent, domain.PublicationRefDisagrees, "", "",
			"the ref creation reported no success, and the read afterwards found a ref carrying a different tree", false); e != nil {
			return e
		}
		return fmt.Errorf("%w: the ref exists and carries a different tree", application.ErrEffectUndecidable)
	case refAbsent:
		// The write did not land. Nothing is recorded as observed and the
		// protocol retries the same operation id.
		return fmt.Errorf("%w: the ref creation did not complete", ErrForgePublishIncomplete)
	default:
		return fmt.Errorf("%w: the ref creation did not complete and the ref could not be read", application.ErrEffectUndecidable)
	}
}

// Observe is the EffectObserver half: it is called for an item the protocol
// already marked ambiguous, and it answers only from reads.
func (p ForgePublisher) Observe(ctx context.Context, delivery application.EffectDelivery) (application.OutboxObservation, error) {
	if err := p.configured(); err != nil {
		return application.ObservationUnknown, err
	}
	intent, err := publicationIntent(delivery)
	if err != nil {
		return application.ObservationUnknown, err
	}
	owner, name := intent.Locator.Owner, intent.Locator.Name
	readBack, state, err := p.readRef(ctx, owner, name, intent.Ref)
	if err != nil {
		return application.ObservationUnknown, err
	}
	switch state {
	case refPresent:
		commit, e := p.readCommit(ctx, owner, name, readBack.Object.SHA)
		if e != nil {
			return application.ObservationUnknown, e
		}
		if commit.Tree.SHA == intent.HeadTree && len(commit.Parents) == 1 && commit.Parents[0].SHA == intent.BaseCommit {
			if e = p.record(ctx, delivery, intent, domain.PublicationPublishedAndObserved, commit.SHA, commit.Tree.SHA,
				"the read after an undecidable delivery found the intended ref with the intended tree and parent", true); e != nil {
				return application.ObservationUnknown, e
			}
			return application.ObservationConfirmed, nil
		}
		if e = p.record(ctx, delivery, intent, domain.PublicationRefDisagrees, "", "",
			"the read after an undecidable delivery found a ref carrying a different tree; this needs a person", false); e != nil {
			return application.ObservationUnknown, e
		}
		return application.ObservationUnknown, nil
	case refAbsent:
		// Absence is a measurement: the effect did not land, so the same
		// operation id may be retried through the normal claim path.
		return application.ObservationNotObserved, nil
	default:
		// The read itself did not complete. Returning an error leaves the item
		// ambiguous for the next pass rather than converting a transport
		// failure into a permanent needs-input.
		return application.ObservationUnknown, fmt.Errorf("%w: the ref could not be read", ErrForgePublishIncomplete)
	}
}
