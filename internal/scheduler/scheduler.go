// Package scheduler decides from a bounded snapshot; persistence revalidates
// and applies the returned plan transactionally.
package scheduler

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

const (
	MaxCandidates         = 100
	FailureStormThreshold = 3

	// StarvationBoundTicks is the maximum number of Decide/Apply ticks a
	// waiting, high-value Requirement in one Repository may go unassigned
	// while a second Repository's failure-and-retry flood keeps consuming
	// all runner capacity, once the waiting Requirement's
	// domain.PriorityAssessment.StarvationRisk is being raised on each
	// re-assessment (V2-030 / A5). It is derived from priority.go's
	// documented weight table, not tuned to whatever the implementation
	// happens to do: weightStarvationRisk (500) is the largest weight of
	// all, so a handful of StarvationRisk increments is guaranteed to
	// eventually close any single fixed competing factor; 5 gives
	// starvation_test.go's specific scenario a one-tick margin over the
	// tick on which it actually converges. See starvation_test.go for the
	// worked numbers and the negative control that must NOT converge
	// inside this same bound once the term is neutralised.
	StarvationBoundTicks = 5
)

type RequirementStatus string

const (
	StatusReady     RequirementStatus = "ready"
	StatusAssigned  RequirementStatus = "assigned"
	StatusCompleted RequirementStatus = "completed"
)

type AccessMode string

const (
	Read  AccessMode = "read"
	Write AccessMode = "write"
)

type ResourceRequest struct {
	Name   string
	Mode   AccessMode
	Global bool
}
type Requirement struct {
	ID            string
	RepositoryIDs []string
	Priority      int64
	CreatedAt     time.Time
	Dependencies  []string
	Status        RequirementStatus
	Resources     []ResourceRequest

	// Assessment is optional. When nil, Decide scores this candidate with
	// the legacy scalar Priority/age formula, byte-identical to the
	// pre-V2-030 scheduler. When set, Decide scores it with the
	// multi-factor formula in priority.go instead, and
	// Assessment.Executable == false excludes it from assignment
	// altogether regardless of score.
	Assessment *domain.PriorityAssessment
}
type Repository struct {
	ID            string
	FailureCount  int
	IsolatedUntil time.Time
}
type Runner struct {
	ID, Provider     string
	Capacity, Active int
}
type Claim struct {
	Resource, Owner, RepositoryID string
	Mode                          AccessMode
}
type Assignment struct {
	RequirementID, RepositoryID, RunnerID, Provider string
	Resources                                       []ResourceRequest
}
type Snapshot struct {
	Requirements     []Requirement
	Repositories     []Repository
	Runners          []Runner
	Claims           []Claim
	ProviderCapacity map[string]int
	Now              time.Time
	CandidateLimit   int
}
type Plan struct {
	Assignments []Assignment
	Claims      []Claim

	// Decisions carries one auditable Decision record per candidate Decide
	// considered, in the same order the sorted candidate ranking visited
	// them (V2-030 / A4). Existing tests that compare Plan values by field
	// (Assignments, Claims) are unaffected; nothing compares whole Plan
	// values, which would turn this into a formatting assertion instead of
	// a behavioural one.
	Decisions []Decision
}

var (
	ErrCandidateLimit  = errors.New("scheduler candidate limit exceeded")
	ErrConflict        = errors.New("scheduler resource conflict")
	ErrAtomicPlan      = errors.New("cross-repository plan cannot be applied atomically")
	ErrDependencyCycle = errors.New("scheduler dependency cycle")
)

func (s Snapshot) limit() int {
	if s.CandidateLimit <= 0 || s.CandidateLimit > MaxCandidates {
		return MaxCandidates
	}
	return s.CandidateLimit
}
func Decide(s Snapshot) (Plan, error) {
	if s.Now.IsZero() {
		return Plan{}, errors.New("scheduler time is required")
	}
	if len(s.Requirements) > s.limit() {
		return Plan{}, ErrCandidateLimit
	}
	if cyclic(s.Requirements) {
		return Plan{}, ErrDependencyCycle
	}
	repos := map[string]Repository{}
	for _, r := range s.Repositories {
		if r.ID == "" {
			return Plan{}, errors.New("repository id is required")
		}
		repos[r.ID] = r
	}
	done := map[string]bool{}
	for _, q := range s.Requirements {
		if q.Status == StatusCompleted {
			done[q.ID] = true
		}
	}
	runners := append([]Runner(nil), s.Runners...)
	sort.Slice(runners, func(i, j int) bool { return runners[i].ID < runners[j].ID })
	providerActive := map[string]int{}
	for _, r := range runners {
		providerActive[r.Provider] += r.Active
	}
	items := append([]Requirement(nil), s.Requirements...)
	sort.SliceStable(items, func(i, j int) bool {
		a, b := scoreFn(items[i], s.Now), scoreFn(items[j], s.Now)
		return a > b || (a == b && items[i].ID < items[j].ID)
	})
	plan := Plan{}
	for rank, q := range items {
		reason, ready := decideReason(q, done, repos, s.Now, s.Claims, plan.Claims)
		assigned := false
		if ready {
			if ri := chooseRunner(runners, providerActive, s.ProviderCapacity); ri < 0 {
				reason = ReasonNoRunnerCapacity
			} else {
				r := runners[ri]
				for _, repo := range unique(q.RepositoryIDs) {
					plan.Assignments = append(plan.Assignments, Assignment{q.ID, repo, r.ID, r.Provider, append([]ResourceRequest(nil), q.Resources...)})
					for _, resource := range q.Resources {
						claimRepository := repo
						if resource.Global {
							claimRepository = ""
						}
						plan.Claims = append(plan.Claims, Claim{resource.Name, q.ID, claimRepository, resource.Mode})
					}
				}
				runners[ri].Active++
				providerActive[r.Provider]++
				assigned = true
			}
		}
		plan.Decisions = append(plan.Decisions, newDecision(q, s.Now, rank, assigned, reason))
	}
	return plan, nil
}

// structurallyReady reports whether q is well-formed and in a schedulable
// status on its own terms, independent of its dependencies, its
// Repositories' health, existing Claims, or runner capacity: a non-empty ID,
// Status == StatusReady, at least one RepositoryID, and every ResourceRequest
// well-formed (non-empty name, Mode is Read or Write).
func structurallyReady(q Requirement) bool {
	if q.ID == "" || q.Status != StatusReady || len(unique(q.RepositoryIDs)) == 0 {
		return false
	}
	for _, r := range q.Resources {
		if r.Name == "" || (r.Mode != Read && r.Mode != Write) {
			return false
		}
	}
	return true
}

// dependenciesMet reports whether every one of q's Dependencies is
// StatusCompleted.
func dependenciesMet(q Requirement, done map[string]bool) bool {
	for _, d := range q.Dependencies {
		if !done[d] {
			return false
		}
	}
	return true
}
func repositoriesAvailable(q Requirement, repos map[string]Repository, now time.Time) bool {
	for _, id := range unique(q.RepositoryIDs) {
		r, ok := repos[id]
		if !ok || r.FailureCount >= FailureStormThreshold || (!r.IsolatedUntil.IsZero() && now.Before(r.IsolatedUntil)) {
			return false
		}
	}
	return true
}
func owned(q Requirement, existing, planned []Claim) bool {
	for _, c := range append(append([]Claim(nil), existing...), planned...) {
		if c.Owner == q.ID {
			return true
		}
	}
	return false
}
func resourceConflict(q Requirement, existing, planned []Claim) bool {
	claims := append(append([]Claim(nil), existing...), planned...)
	for _, repo := range unique(q.RepositoryIDs) {
		for _, r := range q.Resources {
			targetRepository := repo
			if r.Global {
				targetRepository = ""
			}
			for _, c := range claims {
				if c.RepositoryID == targetRepository && c.Resource == r.Name && (c.Mode == Write || r.Mode == Write) {
					return true
				}
			}
		}
	}
	return false
}
func chooseRunner(rs []Runner, active, capacity map[string]int) int {
	best := -1
	for i, r := range rs {
		if r.ID == "" || r.Provider == "" || r.Capacity <= 0 || r.Active >= r.Capacity {
			continue
		}
		if cap, ok := capacity[r.Provider]; ok && active[r.Provider] >= cap {
			continue
		}
		if best < 0 || r.Active < rs[best].Active {
			best = i
		}
	}
	return best
}
func unique(vs []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range vs {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
func cyclic(requirements []Requirement) bool {
	edges := map[string][]string{}
	for _, q := range requirements {
		edges[q.ID] = append([]string(nil), q.Dependencies...)
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return true
		}
		if visited[id] {
			return false
		}
		visiting[id] = true
		for _, d := range edges[id] {
			if _, ok := edges[d]; ok && visit(d) {
				return true
			}
		}
		visiting[id] = false
		visited[id] = true
		return false
	}
	for id := range edges {
		if visit(id) {
			return true
		}
	}
	return false
}
func Apply(s Snapshot, p Plan) (Snapshot, error) {
	next := s
	next.Requirements = append([]Requirement(nil), s.Requirements...)
	next.Claims = append([]Claim(nil), s.Claims...)
	by := map[string]int{}
	for _, a := range p.Assignments {
		by[a.RequirementID]++
	}
	for _, q := range s.Requirements {
		if n := by[q.ID]; n > 0 && n != len(unique(q.RepositoryIDs)) {
			return s, ErrAtomicPlan
		}
	}
	for _, c := range p.Claims {
		for _, old := range next.Claims {
			if old.RepositoryID == c.RepositoryID && old.Resource == c.Resource && old.Owner != c.Owner && (old.Mode == Write || c.Mode == Write) {
				return s, fmt.Errorf("%w: %s", ErrConflict, c.Resource)
			}
		}
		next.Claims = append(next.Claims, c)
	}
	for i := range next.Requirements {
		if by[next.Requirements[i].ID] > 0 {
			next.Requirements[i].Status = StatusAssigned
		}
	}
	return next, nil
}
