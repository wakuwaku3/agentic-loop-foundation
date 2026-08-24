// Pipeline orchestrates the local, gate-checked promotion path: re-verify a
// bundle against its source tree, re-derive the capability set from the
// compiled contract, run the existing candidate/evidence gate, and only
// then move the route. No step here trusts a caller-supplied digest
// (dp-v2-021 d2, d4 L1); PromotionGate itself is unchanged (release_test.go
// still calls it directly and still passes unedited).
package release

import (
	"fmt"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// Pipeline ties the package's existing Store port and Router to the source
// tree a bundle claims to have been assembled from. There is no canonical
// release aggregate here: internal/application, internal/store and
// internal/api are prohibited for this task (escalation E2), so Pipeline is
// the local-closure substitute, not a replacement for one.
type Pipeline struct {
	Store  Store
	Router *Router
	Root   string
}

// NewPipeline constructs a Pipeline over an existing Store and Router,
// verifying against the source tree at root.
func NewPipeline(store Store, router *Router, root string) *Pipeline {
	return &Pipeline{Store: store, Router: router, Root: root}
}

// Promote re-verifies bundle.Members against the source tree at p.Root,
// re-derives the compiled contract from that same tree and refuses a
// candidate whose capability set diverges from it, then runs the existing
// PromotionGate. Only on success does it persist the bundle and advance the
// route; on any failure the route (and its Generation) is left untouched.
func (p *Pipeline) Promote(bundle Bundle, control domain.EffectiveControlResult, permit domain.PermitDecision) (GateResult, error) {
	if err := VerifySource(bundle.Members, p.Root); err != nil {
		return GateResult{}, fmt.Errorf("promotion refused: source verification: %w", err)
	}
	if err := VerifyCandidateDigests(bundle.Candidate, bundle.Members); err != nil {
		return GateResult{}, fmt.Errorf("promotion refused: %w", err)
	}
	assembled, err := AssembleFromRoot(p.Root)
	if err != nil {
		return GateResult{}, fmt.Errorf("promotion refused: re-assemble source tree: %w", err)
	}
	if err := VerifyCandidateAgainstContract(bundle.Candidate, assembled.Contract); err != nil {
		return GateResult{}, fmt.Errorf("promotion refused: candidate capability set diverges from contract: %w", err)
	}
	result, err := PromotionGate(bundle, control, permit)
	if err != nil {
		return GateResult{}, err
	}
	if err := p.Store.Put(bundle); err != nil {
		return GateResult{}, fmt.Errorf("store bundle: %w", err)
	}
	if err := p.Router.Promote(bundle.Repository, bundle.Candidate.CandidateDigest); err != nil {
		return GateResult{}, fmt.Errorf("advance route: %w", err)
	}
	return result, nil
}

// RetentionInput is the injected-clock, contract-window state
// RetentionEligible needs. Nothing here reads the wall clock.
type RetentionInput struct {
	Digest                  string
	CurrentStable           string
	PreviousStable          string
	RollbackWindow          time.Duration
	RolledBackAt            time.Time // zero if this digest was never rolled back away from
	Now                     time.Time
	ReferencedByRequirement bool
}

// RetentionEligible is a pure function with three independent refusals and
// one positive case (dp-v2-021 d11, A10): a version is eligible for
// retention (GC execution stays with V2-034 at M8) only when it is neither
// the current nor the previous stable route target, its rollback window (an
// injected clock against a contract-declared duration, never the clock
// alone) has closed, and no Requirement's StableSnapshot references it.
func RetentionEligible(input RetentionInput) (bool, string) {
	if input.Digest == input.CurrentStable {
		return false, "version is the current stable route target"
	}
	if input.Digest == input.PreviousStable {
		return false, "version is the previous stable route target"
	}
	if !input.RolledBackAt.IsZero() && input.Now.Before(input.RolledBackAt.Add(input.RollbackWindow)) {
		return false, "rollback window is still open"
	}
	if input.ReferencedByRequirement {
		return false, "a Requirement's StableSnapshot still references this version"
	}
	return true, ""
}
