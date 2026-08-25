// Package provider is the provider-neutral boundary for local AI CLIs.
// Adapters may translate this contract to a provider command, but no caller
// receives credentials or raw provider conversation through this package.
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

const (
	ContractVersion = "v1"
	MaxPacketBytes  = 1 << 20
	MaxFixtureBytes = 1 << 20
)

var (
	ErrInvalidRequest = errors.New("invalid provider request")
	ErrInvalidPacket  = errors.New("invalid work packet")
	ErrInvalidFixture = errors.New("invalid provider fixture")
)

type Decision struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}
type Artifact struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}
type Verification struct {
	Name           string `json:"name"`
	Status         string `json:"status"`
	EvidenceDigest string `json:"evidence_digest,omitempty"`
}

// WorkPacket is the only context that may cross a provider handoff. Its shape
// is deliberately facts-and-evidence only: there is no prompt, conversation,
// credential, token, or provider response field.
type WorkPacket struct {
	Version            string         `json:"version"`
	RequirementID      string         `json:"requirement_id"`
	RequirementSummary string         `json:"requirement_summary"`
	Repository         string         `json:"repository,omitempty"`
	IncrementID        string         `json:"increment_id"`
	Constraints        []string       `json:"constraints,omitempty"`
	Decisions          []Decision     `json:"decisions,omitempty"`
	Artifacts          []Artifact     `json:"artifacts,omitempty"`
	Verification       []Verification `json:"verification,omitempty"`
	Unresolved         []string       `json:"unresolved,omitempty"`
}

func (p WorkPacket) Validate() error {
	if p.Version != ContractVersion || p.RequirementID == "" || p.IncrementID == "" || strings.TrimSpace(p.RequirementSummary) == "" {
		return ErrInvalidPacket
	}
	b, err := json.Marshal(p)
	if err != nil || len(b) > MaxPacketBytes {
		return ErrInvalidPacket
	}
	if forbidden.MatchString(strings.ToLower(string(b))) || secret.MatchString(string(b)) {
		return ErrInvalidPacket
	}
	return nil
}

type Request struct {
	OperationID string        `json:"operation_id"`
	Workspace   string        `json:"workspace"`
	Packet      WorkPacket    `json:"packet"`
	Model       string        `json:"model,omitempty"`
	Timeout     time.Duration `json:"-"`
}

func (r Request) Validate() error {
	if r.OperationID == "" || r.Workspace == "" {
		return ErrInvalidRequest
	}
	if err := r.Packet.Validate(); err != nil {
		return err
	}
	if strings.Contains(r.Workspace, "\x00") || strings.ContainsAny(r.Workspace, "\r\n") {
		return ErrInvalidRequest
	}
	if r.Timeout < 0 {
		return ErrInvalidRequest
	}
	return nil
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	TotalTokens  int64 `json:"total_tokens,omitempty"`
	Limit        int64 `json:"limit,omitempty"`
}
type FailureClass string

const (
	FailureInvalidInput FailureClass = "invalid-input"
	FailureTransport    FailureClass = "provider-transport"
	FailureModel        FailureClass = "provider-model"
	FailureQuota        FailureClass = "provider-quota"
	FailureTimeout      FailureClass = "timeout"
	FailureCancelled    FailureClass = "cancelled"
	FailureContract     FailureClass = "contract-incompatible"
	FailureUnknown      FailureClass = "unknown"
	// FailureUnauthenticated is the class of a CLI that is installed and
	// reachable but has no authenticated session on this machine (dp-v2-067
	// d9). Without it the first invocation of an un-logged-in provider is
	// classified FailureTransport, which reads as an infrastructure fault and
	// sends the owner looking in the wrong place -- so the one state that
	// actually blocks the milestone would be invisible in the exact record
	// meant to reveal it. It is deliberately NOT retryable: retrying cannot
	// produce a session, because authenticating a CLI uses the owner's own
	// identity and no agent can perform it.
	FailureUnauthenticated FailureClass = "provider-unauthenticated"
)

type Failure struct {
	Class     FailureClass `json:"class"`
	Code      string       `json:"code,omitempty"`
	Retryable bool         `json:"retryable"`
	Ambiguous bool         `json:"ambiguous"`
	Message   string       `json:"message,omitempty"`
	Usage     Usage        `json:"usage,omitempty"`
}
type Result struct {
	Provider     string   `json:"provider"`
	Model        string   `json:"model,omitempty"`
	Succeeded    bool     `json:"succeeded"`
	Checkpoint   string   `json:"checkpoint,omitempty"`
	OutputDigest string   `json:"output_digest,omitempty"`
	Usage        Usage    `json:"usage,omitempty"`
	Failure      *Failure `json:"failure,omitempty"`
}

type Adapter interface {
	Name() string
	Build(Request) (Invocation, error)
	Parse([]byte) (Result, error)
	NormalizeError(error) Failure
}
type Provider interface {
	Run(context.Context, Request) (Result, error)
}
type Invocation struct {
	Argv             []string
	Stdin            []byte
	WorkingDirectory string
	Environment      []string
}

var forbidden = regexp.MustCompile(`(?i)(raw[_ -]?conversation|raw[_ -]?prompt|credential|password|private[_ -]?key|api[_ -]?key)`)
var secret = regexp.MustCompile(`(?i)(bearer\s+[a-z0-9._~+/=-]{12,}|-----begin [a-z ]+private key-----|gh[pousr]_[a-z0-9]{20,})`)

func classify(err error) Failure {
	if err == nil {
		return Failure{}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return Failure{Class: FailureCancelled, Message: "provider run cancelled"}
	case errors.Is(err, context.DeadlineExceeded):
		return Failure{Class: FailureTimeout, Retryable: true, Ambiguous: true, Message: "provider run timed out"}
	}
	return Failure{Class: FailureTransport, Retryable: true, Message: safeMessage(err.Error())}
}
func safeMessage(s string) string {
	if len(s) > 256 {
		s = s[:256]
	}
	if secret.MatchString(s) {
		return "provider failure (secret redacted)"
	}
	return s
}
func digest(value string) string {
	h := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(h[:])
}
func (f Failure) Error() string {
	if f.Class == "" {
		return ""
	}
	return string(f.Class) + ": " + f.Message
}
func (r Result) Validate() error {
	if r.Provider == "" {
		return ErrInvalidFixture
	}
	if r.Failure != nil && r.Succeeded {
		return ErrInvalidFixture
	}
	if r.Usage.InputTokens < 0 || r.Usage.OutputTokens < 0 || r.Usage.TotalTokens < 0 || r.Usage.Limit < 0 {
		return ErrInvalidFixture
	}
	return nil
}

// ClassifyError is exported for process supervisors and keeps cancellation
// and timeout semantics identical across all adapters.
func ClassifyError(err error) Failure { return classify(err) }

// DigestOutput is the only permitted way to retain provider output metadata.
func DigestOutput(output string) string { return digest(output) }
