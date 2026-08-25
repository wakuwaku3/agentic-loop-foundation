package application

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// MaxPageSize is a hard safety limit for owner read models and exports.
const MaxPageSize = 100

var ErrInvalidCursor = errors.New("invalid or expired cursor")

type cursorPayload struct {
	Version string `json:"v"`
	After   string `json:"after"`
}

// Cursors intentionally carry no storage path or tenant information. They
// are opaque to clients and can safely be invalidated by changing the
// version. The adapter still receives only the decoded ordering key.
func encodeCursor(after string) string {
	b, _ := json.Marshal(cursorPayload{Version: "v1", After: after})
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeCursor(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", ErrInvalidCursor
	}
	if len(b) > 1024 || !utf8.Valid(b) {
		return "", ErrInvalidCursor
	}
	var p cursorPayload
	if json.Unmarshal(b, &p) != nil || p.Version != "v1" || p.After == "" || len(p.After) > 512 || !utf8.ValidString(p.After) || strings.TrimSpace(p.After) == "" {
		return "", ErrInvalidCursor
	}
	return p.After, nil
}

type RequirementPage struct {
	Requirements []RequirementView `json:"requirements"`
	NextCursor   string            `json:"next_cursor,omitempty"`
	PageSize     int               `json:"page_size"`
}

type RequirementIncrementView struct {
	IncrementID string                 `json:"increment_id"`
	Status      domain.IncrementStatus `json:"status"`
	Version     domain.Version         `json:"version"`
	Executions  []ExecutionView        `json:"executions,omitempty"`
}
type ExecutionView struct {
	ExecutionID string                 `json:"execution_id"`
	Status      domain.ExecutionStatus `json:"status"`
	Version     domain.Version         `json:"version"`
	RunnerID    string                 `json:"runner_id,omitempty"`
}
type RequirementDetailView struct {
	RequirementID string                     `json:"requirement_id"`
	OriginalText  string                     `json:"original_text,omitempty"`
	Status        domain.RequirementStatus   `json:"status"`
	Version       domain.Version             `json:"version"`
	Increments    []RequirementIncrementView `json:"increments"`
	NextAction    string                     `json:"next_action"`
	PageSize      int                        `json:"page_size"`
	Truncated     bool                       `json:"truncated"`
	RequestedBy   *domain.RequestedBy        `json:"requested_by,omitempty"`
	// RepositoryID is the Repository this Requirement is linked to, read
	// from the write-once Requirement-to-Repository link (V2-071 A12). An
	// unlinked Requirement omits the field entirely: an absent association
	// is reported as absent, never as a guessed or defaulted repository.
	RepositoryID string `json:"repository_id,omitempty"`
	// CapturedAt is the instant the Requirement was captured (V2-073 A11).
	// It is a pointer so a legacy Requirement -- one written before the field
	// existed -- omits the key entirely instead of reporting the zero
	// instant, which would marshal to a real-looking date in the year 1 and
	// would be read by an ordering rule that rewards age as maximally old.
	CapturedAt *time.Time `json:"captured_at,omitempty"`
}

// RequirementView remains compatible with the original v1 response. Text is
// retained as an alias while new clients should use original_text on detail.
type RequirementView struct {
	RequirementID string                   `json:"requirement_id"`
	Status        domain.RequirementStatus `json:"status"`
	Version       domain.Version           `json:"version"`
	IncrementIDs  []string                 `json:"increment_ids"`
	Text          string                   `json:"text,omitempty"`
	RequestedBy   *domain.RequestedBy      `json:"requested_by,omitempty"`
	// RepositoryID follows RequirementDetailView's field of the same name:
	// present only when a link was actually read, omitted otherwise.
	RepositoryID string `json:"repository_id,omitempty"`
	// CapturedAt follows RequirementDetailView's field of the same name: the
	// recorded capture instant, or the key omitted entirely for a legacy
	// Requirement that has none.
	CapturedAt *time.Time `json:"captured_at,omitempty"`
}

func boundPageSize(size int) (int, error) {
	if size == 0 {
		return 25, nil
	}
	if size < 0 || size > MaxPageSize {
		return 0, fmt.Errorf("page_size must be between 1 and %d", MaxPageSize)
	}
	return size, nil
}

// requestedByView returns nil for a value that was never recorded (a legacy
// Requirement, or a Control Intent revision with no side-table row), so the
// JSON response omits requested_by entirely rather than emitting an empty
// object. A copy is returned so callers cannot mutate the stored value
// through the pointer.
func requestedByView(rb domain.RequestedBy) *domain.RequestedBy {
	if !rb.Recorded() {
		return nil
	}
	v := rb
	return &v
}

// capturedAtView mirrors requestedByView for the capture time (V2-073 A6),
// and its single source is the Requirement record handed to it: there is no
// clock read, no fallback to "now" and no scan of the event log here or
// anywhere on the read path. A legacy Requirement -- CaptureRecorded() false,
// i.e. a record written before the field existed -- yields nil, so the
// response omits captured_at entirely rather than emitting
// 0001-01-01T00:00:00Z. That matters more here than it does for attribution:
// the zero instant reads as a real instant in the year 1, and an ordering
// rule that rewards age would read every legacy record as maximally old and
// therefore maximally privileged. Omitting the key forces every consumer to
// decide what an absent capture time means instead of silently inheriting an
// unbounded age. A copy is returned so a caller cannot mutate stored state
// through the pointer.
func capturedAtView(r domain.Requirement) *time.Time {
	if !r.CaptureRecorded() {
		return nil
	}
	v := r.CapturedAt
	return &v
}

// requirementViews projects rows onto the wire shape. links is the batch link
// read for exactly the ids on this page; a row absent from it carries no
// repository_id at all, which is what distinguishes "not linked" from "linked
// to the empty string".
func requirementViews(rows []domain.Requirement, texts map[string]string, links map[string]domain.RequirementRepositoryLink) []RequirementView {
	out := make([]RequirementView, 0, len(rows))
	for _, r := range rows {
		ids := make([]string, len(r.Increments))
		for i, id := range r.Increments {
			ids[i] = id.String()
		}
		out = append(out, RequirementView{RequirementID: r.ID.String(), Status: r.Status, Version: r.Version, IncrementIDs: ids, Text: texts[r.ID.String()], RequestedBy: requestedByView(r.RequestedBy), RepositoryID: links[r.ID.String()].RepositoryID.String(), CapturedAt: capturedAtView(r)})
	}
	return out
}

func (s *Service) ListRequirementsPage(ctx context.Context, cursor string, pageSize int) (RequirementPage, error) {
	if _, _, err := callerActor(ctx, RoleOwner); err != nil {
		return RequirementPage{}, err
	}
	limit, err := boundPageSize(pageSize)
	if err != nil {
		return RequirementPage{}, err
	}
	after, err := decodeCursor(cursor)
	if err != nil {
		return RequirementPage{}, err
	}
	var page RequirementPage
	err = s.transact(ctx, func(u UnitOfWork) error {
		var rows []domain.Requirement
		var more bool
		rows, more, err = u.RequirementsPage(ctx, after, limit)
		if err != nil {
			return err
		}
		ids := make([]string, len(rows))
		for i := range rows {
			ids[i] = rows[i].ID.String()
		}
		texts := map[string]string{}
		texts, err = u.RequirementTexts(ctx, ids)
		if err != nil {
			return err
		}
		links := map[string]domain.RequirementRepositoryLink{}
		links, err = u.RequirementRepositoryLinks(ctx, ids)
		if err != nil {
			return err
		}
		page.Requirements = requirementViews(rows, texts, links)
		page.PageSize = limit
		if more && len(rows) != 0 {
			page.NextCursor = encodeCursor(rows[len(rows)-1].ID.String())
		}
		return nil
	})
	return page, err
}

func (s *Service) GetRequirementDetail(ctx context.Context, id string) (RequirementDetailView, bool, error) {
	if _, _, err := callerActor(ctx, RoleOwner); err != nil {
		return RequirementDetailView{}, false, err
	}
	var out RequirementDetailView
	var found bool
	err := s.transact(ctx, func(u UnitOfWork) error {
		r, ok, err := u.Requirement(ctx, id)
		if err != nil || !ok {
			found = ok
			return err
		}
		found = true
		text, _, err := u.RequirementText(ctx, id)
		if err != nil {
			return err
		}
		link, hasLink, err := u.RequirementRepositoryLink(ctx, id)
		if err != nil {
			return err
		}
		out = RequirementDetailView{RequirementID: id, OriginalText: text, Status: r.Status, Version: r.Version, Increments: []RequirementIncrementView{}, PageSize: MaxPageSize, RequestedBy: requestedByView(r.RequestedBy), CapturedAt: capturedAtView(r)}
		if hasLink {
			out.RepositoryID = link.RepositoryID.String()
		}
		incs := make([]domain.Increment, 0, len(r.Increments))
		incrementIDs := r.Increments
		if len(incrementIDs) > MaxPageSize {
			incrementIDs = incrementIDs[:MaxPageSize]
			out.Truncated = true
		}
		incIDs := make([]string, len(incrementIDs))
		for i := range incrementIDs {
			incIDs[i] = incrementIDs[i].String()
		}
		incs, err = u.IncrementsForRequirements(ctx, incIDs)
		if err != nil {
			return err
		}
		incIDsForExec := make([]string, len(incs))
		for i := range incs {
			incIDsForExec[i] = incs[i].ID.String()
		}
		execs := []domain.Execution{}
		execs, err = u.ExecutionsForIncrements(ctx, incIDsForExec)
		if err != nil {
			return err
		}
		byInc := map[string][]ExecutionView{}
		for _, e := range execs {
			byInc[e.IncrementID.String()] = append(byInc[e.IncrementID.String()], ExecutionView{ExecutionID: e.ID.String(), Status: e.Status, Version: e.Version, RunnerID: e.RunnerID.String()})
		}
		for _, inc := range incs {
			out.Increments = append(out.Increments, RequirementIncrementView{IncrementID: inc.ID.String(), Status: inc.Status, Version: inc.Version, Executions: byInc[inc.ID.String()]})
		}
		out.NextAction = nextAction(r.Status, incs, execs)
		return nil
	})
	return out, found, err
}

func nextAction(status domain.RequirementStatus, incs []domain.Increment, execs []domain.Execution) string {
	if status == domain.RequirementCompleted || status == domain.RequirementCancelled {
		return "none"
	}
	for _, e := range execs {
		if e.Status == domain.ExecutionRunning || e.Status == domain.ExecutionStarting {
			return "monitor execution"
		}
	}
	for _, i := range incs {
		switch i.Status {
		case domain.IncrementFailed:
			return "review failed increment"
		case domain.IncrementPreviewValidating, domain.IncrementAccepted:
			return "verify release"
		case domain.IncrementReady, domain.IncrementProposed:
			return "run next increment"
		}
	}
	if len(incs) == 0 {
		return "plan increments"
	}
	return "review requirement"
}

type ControlReadModel struct {
	Scope          domain.ControlScope      `json:"scope"`
	Mode           domain.ControlMode       `json:"mode"`
	Revision       domain.Revision          `json:"revision"`
	Requested      bool                     `json:"requested"`
	Acknowledged   bool                     `json:"acknowledged"`
	Effective      bool                     `json:"effective"`
	Verified       bool                     `json:"verified"`
	At             string                   `json:"at,omitempty"`
	Reason         string                   `json:"reason,omitempty"`
	RequestedAt    string                   `json:"requested_at,omitempty"`
	AcknowledgedAt string                   `json:"acknowledged_at,omitempty"`
	EffectiveAt    string                   `json:"effective_at,omitempty"`
	VerifiedAt     string                   `json:"verified_at,omitempty"`
	EvidenceRef    string                   `json:"evidence_ref,omitempty"`
	Verification   domain.VerificationState `json:"verification"`
	RequestedBy    *domain.RequestedBy      `json:"requested_by,omitempty"`
}

func (s *Service) ListControls(ctx context.Context, limit int) ([]ControlReadModel, error) {
	if _, _, err := callerActor(ctx, RoleOwner); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 25
	}
	if limit > MaxPageSize {
		return nil, fmt.Errorf("page_size must be at most %d", MaxPageSize)
	}
	var out []ControlReadModel
	err := s.transact(ctx, func(u UnitOfWork) error {
		rows, err := u.Controls(ctx)
		if err != nil {
			return err
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Revision > rows[j].Revision })
		if len(rows) > limit {
			rows = rows[:limit]
		}
		for _, c := range rows {
			progress := domain.ControlProgress{State: domain.ControlRequested, RequestedAt: c.At}
			if v, found, e := u.ControlProgress(ctx, c.Revision); e != nil {
				return e
			} else if found {
				progress = v
			}
			var reqBy domain.RequestedBy
			if v, found, e := u.ControlRequestedBy(ctx, c.Revision); e != nil {
				return e
			} else if found {
				reqBy = v
			}
			out = append(out, controlRead(c, progress, reqBy))
		}
		return nil
	})
	return out, err
}
func controlRead(c domain.ControlIntent, p domain.ControlProgress, reqBy domain.RequestedBy) ControlReadModel {
	f := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339Nano)
	}
	effectiveAt := p.EffectiveAt
	if effectiveAt.IsZero() {
		effectiveAt = c.EffectiveAt
	}
	return ControlReadModel{Scope: c.Scope, Mode: c.Mode, Revision: c.Revision, Requested: p.State != "", Acknowledged: p.State == domain.ControlAcknowledged || p.State == domain.ControlEffective || p.State == domain.ControlVerified, Effective: p.State == domain.ControlEffective || p.State == domain.ControlVerified, Verified: p.State == domain.ControlVerified, At: f(c.At), RequestedAt: f(p.RequestedAt), AcknowledgedAt: f(p.AcknowledgedAt), EffectiveAt: f(effectiveAt), VerifiedAt: f(p.VerifiedAt), EvidenceRef: p.EvidenceRef, Verification: p.Verification, Reason: c.Reason, RequestedBy: requestedByView(reqBy)}
}

type QueueSummary struct {
	Requirements        int            `json:"requirements"`
	ByRequirementStatus map[string]int `json:"by_requirement_status"`
	Increments          int            `json:"increments"`
	ByIncrementStatus   map[string]int `json:"by_increment_status"`
	ActiveExecutions    int            `json:"active_executions"`
}

func (s *Service) QueueSummary(ctx context.Context) (QueueSummary, error) {
	if _, _, err := callerActor(ctx, RoleOwner); err != nil {
		return QueueSummary{}, err
	}
	var out QueueSummary
	out.ByRequirementStatus = map[string]int{}
	out.ByIncrementStatus = map[string]int{}
	err := s.transact(ctx, func(u UnitOfWork) error {
		var e error
		out, e = u.QueueSummary(ctx)
		return e
	})
	return out, err
}

type ExportRecord struct {
	SchemaVersion string `json:"schema_version"`
	Kind          string `json:"kind"`
	Digest        string `json:"digest"`
	Value         any    `json:"value"`
}
type ExportRequirement struct {
	RequirementID string                   `json:"requirement_id"`
	Status        domain.RequirementStatus `json:"status"`
	Version       domain.Version           `json:"version"`
	IncrementIDs  []string                 `json:"increment_ids"`
	RequestedBy   *domain.RequestedBy      `json:"requested_by,omitempty"`
}

func makeExportRecord(kind string, value any) (ExportRecord, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return ExportRecord{}, err
	}
	digest := sha256.Sum256(b)
	return ExportRecord{SchemaVersion: "v1", Kind: kind, Digest: fmt.Sprintf("sha256:%x", digest[:]), Value: value}, nil
}

// Export returns bounded, metadata-only records for the owner console. It is
// intentionally not a dump of storage documents: outbox payloads and any
// provider response are excluded at this boundary.
func (s *Service) Export(ctx context.Context, limit int) ([]ExportRecord, error) {
	if _, _, err := callerActor(ctx, RoleOwner); err != nil {
		return nil, err
	}
	if limit < 1 || limit > MaxPageSize {
		return nil, fmt.Errorf("export limit must be between 1 and %d", MaxPageSize)
	}
	out := make([]ExportRecord, 0, limit*2)
	err := s.transact(ctx, func(u UnitOfWork) error {
		rows, more, err := u.RequirementsPage(ctx, "", limit)
		if err != nil {
			return err
		}
		for _, r := range rows {
			ids := make([]string, len(r.Increments))
			for i := range r.Increments {
				ids[i] = r.Increments[i].String()
			}
			v := RequirementView{RequirementID: r.ID.String(), Status: r.Status, Version: r.Version, IncrementIDs: ids, RequestedBy: requestedByView(r.RequestedBy)}
			rec, e := makeExportRecord("requirement", ExportRequirement{RequirementID: v.RequirementID, Status: v.Status, Version: v.Version, IncrementIDs: v.IncrementIDs, RequestedBy: v.RequestedBy})
			if e != nil {
				return e
			}
			out = append(out, rec)
		}
		controls, e := u.Controls(ctx)
		if e != nil {
			return e
		}
		sort.Slice(controls, func(i, j int) bool { return controls[i].Revision > controls[j].Revision })
		if len(controls) > limit {
			controls = controls[:limit]
		}
		for _, v := range controls {
			progress := domain.ControlProgress{State: domain.ControlRequested, RequestedAt: v.At}
			if x, found, e := u.ControlProgress(ctx, v.Revision); e != nil {
				return e
			} else if found {
				progress = x
			}
			var reqBy domain.RequestedBy
			if x, found, e := u.ControlRequestedBy(ctx, v.Revision); e != nil {
				return e
			} else if found {
				reqBy = x
			}
			rec, e := makeExportRecord("control", controlRead(v, progress, reqBy))
			if e != nil {
				return e
			}
			out = append(out, rec)
		}
		events, _, e := u.EventsPage(ctx, "", limit)
		if e != nil {
			return e
		}
		for _, v := range events {
			rec, e := makeExportRecord("event", v)
			if e != nil {
				return e
			}
			out = append(out, rec)
		}
		_ = more
		return nil
	})
	return out, err
}

// ===========================================================================
// Release surface read model (V2-066)
// ===========================================================================
//
// These view types are the serialised shape of GET /v1/release/state. They
// restate nothing: every condition's identity, tri-state, reason and
// deciding source is copied from release.PromotionReport, whose rules live
// in internal/release and internal/domain. The eight promotion conditions
// are not enumerated in this package or in internal/api.
//
// Digests appear here and in the API response. They are written into no
// document under docs/.

// ReleaseRefusalView is one refusal from the promotion authority, kept as its
// own kind so an owner can tell an empty capability set from a missing
// DocsDigest from missing rollback evidence.
type ReleaseRefusalView struct {
	Kind       string `json:"kind"`
	Capability string `json:"capability,omitempty"`
	Reason     string `json:"reason"`
}

// ReleaseConditionView is one of the eight promotion conditions.
type ReleaseConditionView struct {
	ID           string               `json:"id"`
	ContractText string               `json:"contract_text"`
	State        string               `json:"state"`
	Reason       string               `json:"reason"`
	DecidedBy    []string             `json:"decided_by"`
	Refusals     []ReleaseRefusalView `json:"refusals,omitempty"`
}

// ReleaseCandidateIdentityView carries the source-derived identity of the
// candidate this process was assembled from.
type ReleaseCandidateIdentityView struct {
	CandidateID     string `json:"candidate_id"`
	CandidateDigest string `json:"candidate_digest"`
	BundleDigest    string `json:"bundle_digest"`
	ContractDigest  string `json:"contract_digest"`
	DocsDigest      string `json:"docs_digest"`
	EvidenceDigest  string `json:"evidence_digest"`
}

// ReleaseRouteView is this process's own recorded route. Recorded is false
// and Note explains it when no route was recorded; no field is defaulted or
// inferred in that case.
type ReleaseRouteView struct {
	Recorded             bool   `json:"recorded"`
	Source               string `json:"source"`
	Note                 string `json:"note,omitempty"`
	Repository           string `json:"repository,omitempty"`
	PreviewDigest        string `json:"preview_digest,omitempty"`
	StableDigest         string `json:"stable_digest,omitempty"`
	RollbackTargetDigest string `json:"rollback_target_digest,omitempty"`
	RollbackAvailable    bool   `json:"rollback_available"`
	Generation           uint64 `json:"generation"`
}

// ReleaseRollbackView is one recorded rollback.
type ReleaseRollbackView struct {
	Repository string `json:"repository"`
	From       string `json:"from"`
	To         string `json:"to"`
	Reason     string `json:"reason,omitempty"`
	At         string `json:"at"`
}

// ReleaseRetentionView is the bounded canonical-state read behind the
// rollback target: how many Requirements were actually examined, that the
// scan was bounded, and whether a Requirement's StableSnapshot still
// references the target.
//
// The comparison is over both identifiers a StableSnapshot records (its
// ReleaseID and its BundleDigest), because the route names a candidate
// digest while the snapshot records a release id and a bundle digest; a
// match on either counts as a reference. TargetComparedAgainst names those
// fields in the payload so the reader is not left guessing what was compared.
type ReleaseRetentionView struct {
	RequirementsExamined      int      `json:"requirements_examined"`
	ScanBounded               bool     `json:"scan_bounded"`
	PageSize                  int      `json:"page_size"`
	MoreRequirementsExist     bool     `json:"more_requirements_exist"`
	TargetReferenced          bool     `json:"rollback_target_referenced"`
	TargetComparedAgainst     []string `json:"rollback_target_compared_against"`
	ReferencingRequirementIDs []string `json:"referencing_requirement_ids,omitempty"`
}

// ReleaseStateView is the whole owner-readable release surface.
type ReleaseStateView struct {
	SchemaVersion               string                       `json:"schema_version"`
	EnvironmentClass            string                       `json:"environment_class"`
	ReleaseVersion              string                       `json:"release_version"`
	Candidate                   ReleaseCandidateIdentityView `json:"candidate"`
	AssembledAt                 string                       `json:"assembled_at"`
	VersionSource               string                       `json:"version_source"`
	Conditions                  []ReleaseConditionView       `json:"conditions"`
	Promotable                  bool                         `json:"promotable"`
	DeclaredCapabilities        []string                     `json:"declared_capabilities"`
	CapabilitiesWithoutEvidence []string                     `json:"capabilities_without_evidence"`
	Route                       ReleaseRouteView             `json:"route"`
	RollbackHistory             []ReleaseRollbackView        `json:"rollback_history"`
	Retention                   ReleaseRetentionView         `json:"retention"`
	NotObserved                 []string                     `json:"not_observed"`
	ResidualGaps                []string                     `json:"residual_gaps"`
}
