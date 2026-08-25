// The owner-readable release surface (V2-066).
//
// This file wires the release machinery into a read, and nothing else. There
// is no promotion, no rollback and no SetPreview here or on any route above
// it: internal/release's Router and Pipeline are not called to mutate
// anything from this package.
//
// Where the observer is held. The natural home for the observer is a field on
// Service, but internal/application/service.go is outside this task's
// allowed paths (wo-v2-066 A21 forbids editing it), and a struct's fields can
// only be declared in the file that declares the struct. The association
// therefore lives in a process-local registry keyed by the *Service it
// belongs to, written once per Service by AttachReleaseObserver and read
// under an RLock by ReleaseState. No goroutine is started and no clock is
// read by the registry.
//
// Nothing in production wiring attaches an observer yet: cmd/** is also
// outside this task's allowed paths, so a real control plane answers
// GET /v1/release/state with 503 not-configured until a follow-up task
// constructs the observer in cmd/control-plane. That is recorded as a
// residual wiring gap rather than papered over with a default root, because
// a defaulted root would make the process report a version it was not
// assembled from.
package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/release"
)

// ErrReleaseObserverNotConfigured is distinguishable on purpose: the route
// maps exactly this error to 503 with an explicit code, and the response
// reports no version at all rather than a guessed one.
var ErrReleaseObserverNotConfigured = errors.New("release observer is not configured: this process was not given an explicit release source root, so it can report no release version")

var releaseObservers = struct {
	mu sync.RWMutex
	by map[*Service]ReleaseObserver
}{by: map[*Service]ReleaseObserver{}}

// AttachReleaseObserver binds a ReleaseObserver to this Service. It is
// idempotent for the same observer and refuses to replace one silently, so a
// process cannot end up reporting two different assembled versions.
func (s *Service) AttachReleaseObserver(observer ReleaseObserver) error {
	if s == nil {
		return errors.New("cannot attach a release observer to a nil Service")
	}
	if observer == nil {
		return errors.New("cannot attach a nil release observer")
	}
	releaseObservers.mu.Lock()
	defer releaseObservers.mu.Unlock()
	if existing, ok := releaseObservers.by[s]; ok && existing != observer {
		return errors.New("a different release observer is already attached to this Service")
	}
	releaseObservers.by[s] = observer
	return nil
}

// DetachReleaseObserver removes the binding. It exists so a test can restore
// the not-configured state deterministically.
func (s *Service) DetachReleaseObserver() {
	releaseObservers.mu.Lock()
	defer releaseObservers.mu.Unlock()
	delete(releaseObservers.by, s)
}

func (s *Service) releaseObserver() (ReleaseObserver, bool) {
	releaseObservers.mu.RLock()
	defer releaseObservers.mu.RUnlock()
	o, ok := releaseObservers.by[s]
	return o, ok
}

// ===========================================================================
// The source-tree observer
// ===========================================================================

// ReleaseObserverConfig is what constructing an observer requires. Every
// field is explicit: there is no default root, no default repository, no
// default environment class and no clock read.
type ReleaseObserverConfig struct {
	// Root is the release source root this process was assembled from.
	Root string
	// Repository is the routing unit (release-contract.md section 6).
	Repository string
	// EnvironmentClass is the declared Preview environment grade, e.g.
	// preview-local or preview-gcp (release-contract.md section 3).
	EnvironmentClass string
	// Candidate is the part of the candidate assembly cannot derive from
	// source bytes: identity, operational evidence and control state. It is
	// supplied by the caller rather than invented here, so this surface never
	// fabricates capability evidence.
	Candidate release.CandidateInput
	// Router is this process's own route table. A nil Router means this
	// process has recorded no route, which the surface reports as such.
	Router *release.Router
	// AssembledAt is the injected instant the snapshot was assembled.
	AssembledAt time.Time
}

// SourceReleaseObserver assembles the report once, at construction, and
// serves it from memory afterwards. It performs no filesystem access per
// read.
type SourceReleaseObserver struct {
	repository  string
	report      release.PromotionReport
	assembledAt time.Time
	router      *release.Router
}

// NewSourceReleaseObserver reads the source tree at cfg.Root exactly once and
// caches the resulting report.
func NewSourceReleaseObserver(cfg ReleaseObserverConfig) (*SourceReleaseObserver, error) {
	if strings.TrimSpace(cfg.Root) == "" {
		return nil, fmt.Errorf("release observer requires an explicitly configured source root")
	}
	if strings.TrimSpace(cfg.Repository) == "" {
		return nil, fmt.Errorf("release observer requires an explicitly configured repository")
	}
	if strings.TrimSpace(cfg.EnvironmentClass) == "" {
		return nil, fmt.Errorf("release observer requires a declared Preview environment class")
	}
	if cfg.AssembledAt.IsZero() {
		return nil, fmt.Errorf("release observer requires an injected assembly instant")
	}
	assembled, candidate, err := release.AssembleCandidate(cfg.Root, cfg.Candidate)
	if err != nil {
		return nil, fmt.Errorf("assemble the release source root: %w", err)
	}
	report, err := release.BuildPromotionReport(release.ReportInput{
		Root:             cfg.Root,
		Candidate:        candidate,
		Assembled:        assembled,
		AssembledAt:      cfg.AssembledAt,
		EnvironmentClass: cfg.EnvironmentClass,
	})
	if err != nil {
		return nil, err
	}
	return &SourceReleaseObserver{repository: cfg.Repository, report: report, assembledAt: cfg.AssembledAt.UTC(), router: cfg.Router}, nil
}

func (o *SourceReleaseObserver) ReleaseSnapshot() (release.PromotionReport, time.Time) {
	return o.report, o.assembledAt
}
func (o *SourceReleaseObserver) ObservedRepository() string { return o.repository }
func (o *SourceReleaseObserver) RecordedRoute() (release.Route, bool) {
	if o.router == nil {
		return release.Route{}, false
	}
	return o.router.Get(o.repository)
}
func (o *SourceReleaseObserver) RollbackHistory() []release.RollbackRecord {
	if o.router == nil {
		return nil
	}
	return o.router.RollbackHistory(o.repository)
}

// ===========================================================================
// The owner read
// ===========================================================================

// routeSourceStatement is fixed prose, not a measurement: it states whose
// route the payload reports, so the reader is never left to assume it
// describes a deployed environment.
const routeSourceStatement = "this process's own recorded route table; it is not a deployed environment's routing"

const noRouteRecordedNote = "no route recorded: this process has recorded no Preview or Stable route for the observed repository"

// ReleaseState is the owner read behind GET /v1/release/state. It is gated
// exactly as the other owner reads are.
func (s *Service) ReleaseState(ctx context.Context) (ReleaseStateView, error) {
	if _, _, err := callerActor(ctx, RoleOwner); err != nil {
		return ReleaseStateView{}, err
	}
	observer, ok := s.releaseObserver()
	if !ok {
		return ReleaseStateView{}, ErrReleaseObserverNotConfigured
	}
	report, assembledAt := observer.ReleaseSnapshot()
	view := ReleaseStateView{
		SchemaVersion:    "v1",
		EnvironmentClass: report.EnvironmentClass,
		ReleaseVersion:   report.ReleaseVersion,
		Candidate: ReleaseCandidateIdentityView{
			CandidateID:     report.CandidateID,
			CandidateDigest: report.CandidateDigest,
			BundleDigest:    report.BundleDigest,
			ContractDigest:  report.ContractDigest,
			DocsDigest:      report.DocsDigest,
			EvidenceDigest:  report.EvidenceDigest,
		},
		AssembledAt:                 assembledAt.UTC().Format(time.RFC3339Nano),
		VersionSource:               "the source manifest this process was assembled from (release.AssembleFromRoot over the seven bundle roles); no Cloud Run revision, deployed image or update channel pointer is read",
		Promotable:                  report.Promotable,
		DeclaredCapabilities:        append([]string(nil), report.DeclaredCapabilities...),
		CapabilitiesWithoutEvidence: append([]string(nil), report.CapabilitiesWithoutEvidence...),
		NotObserved:                 append([]string(nil), report.NotObserved...),
		ResidualGaps:                append([]string(nil), report.ResidualGaps...),
	}
	if view.DeclaredCapabilities == nil {
		view.DeclaredCapabilities = []string{}
	}
	if view.CapabilitiesWithoutEvidence == nil {
		view.CapabilitiesWithoutEvidence = []string{}
	}
	for _, c := range report.Conditions {
		cv := ReleaseConditionView{ID: string(c.ID), ContractText: c.Contract, State: string(c.State), Reason: c.Reason, DecidedBy: append([]string(nil), c.DecidedBy...)}
		for _, rj := range c.Rejections {
			cv.Refusals = append(cv.Refusals, ReleaseRefusalView{Kind: string(rj.Kind), Capability: rj.Capability, Reason: rj.Reason})
		}
		view.Conditions = append(view.Conditions, cv)
	}

	route, recorded := observer.RecordedRoute()
	view.Route = ReleaseRouteView{Recorded: recorded, Source: routeSourceStatement}
	if !recorded {
		view.Route.Note = noRouteRecordedNote
	} else {
		view.Route.Repository = route.Repository
		view.Route.PreviewDigest = route.PreviewDigest
		view.Route.StableDigest = route.StableDigest
		view.Route.RollbackTargetDigest = route.PreviousStableDigest
		view.Route.RollbackAvailable = route.PreviousStableDigest != ""
		view.Route.Generation = route.Generation
	}

	view.RollbackHistory = []ReleaseRollbackView{}
	for _, rec := range observer.RollbackHistory() {
		view.RollbackHistory = append(view.RollbackHistory, ReleaseRollbackView{
			Repository: rec.Repository, From: rec.From, To: rec.To, Reason: rec.Reason,
			At: rec.At.UTC().Format(time.RFC3339Nano),
		})
	}

	retention, err := s.rollbackTargetRetention(ctx, view.Route.RollbackTargetDigest)
	if err != nil {
		return ReleaseStateView{}, err
	}
	view.Retention = retention
	return view, nil
}

// rollbackTargetRetention answers, from canonical state through the existing
// bounded Requirement page, whether any Requirement's StableSnapshot still
// references the rollback target. It reports how many Requirements it
// actually examined and states that the scan was bounded; it never walks the
// whole collection.
func (s *Service) rollbackTargetRetention(ctx context.Context, target string) (ReleaseRetentionView, error) {
	out := ReleaseRetentionView{
		ScanBounded:           true,
		PageSize:              MaxPageSize,
		TargetComparedAgainst: []string{"requirement.stable_snapshot.release_id", "requirement.stable_snapshot.bundle_digest"},
	}
	err := s.transact(ctx, func(u UnitOfWork) error {
		rows, more, err := u.RequirementsPage(ctx, "", MaxPageSize)
		if err != nil {
			return err
		}
		out.RequirementsExamined = len(rows)
		out.MoreRequirementsExist = more
		if target == "" {
			return nil
		}
		for _, r := range rows {
			if referencesReleaseTarget(r.StableSnapshot, target) {
				out.TargetReferenced = true
				out.ReferencingRequirementIDs = append(out.ReferencingRequirementIDs, r.ID.String())
			}
		}
		return nil
	})
	return out, err
}

// referencesReleaseTarget compares the route's target digest against both
// identifiers a StableSnapshot records. The route names a candidate digest
// while the snapshot records a release id and a bundle digest, so a match on
// either is a reference; nothing is inferred from a non-match beyond "these
// two recorded identifiers do not equal the target".
func referencesReleaseTarget(snapshot domain.StableReleaseSnapshot, target string) bool {
	if target == "" {
		return false
	}
	return string(snapshot.ReleaseID) == target || snapshot.BundleDigest == target
}
