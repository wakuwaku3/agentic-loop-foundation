package application

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/release"
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
	// Truncated reports that the per-page Increment budget bound this answer
	// (V2-095 A6). The list page reads each row's Increments and Executions
	// through two batch ports called ONCE each per page, and the total number
	// of Increment ids collected across the whole page is capped at
	// MaxPageSize. When that cap binds, some row's increments and therefore
	// its next_action were computed over a BOUNDED set, and the page says so
	// here rather than silently dropping rows. It follows
	// RequirementDetailView.Truncated's precedent exactly, including the json
	// tag, so a reader who already understands the detail view's bound
	// understands this one.
	Truncated bool `json:"truncated"`
	// Filter reports the repository_id filter when one was applied (V2-095 A8,
	// escalation E22-7). It is OMITTED ENTIRELY on an unfiltered page: an
	// absent filter is reported as absent, never as a filter on the empty
	// string. When present it also surfaces the bound the
	// RequirementIDsForRepository port already reports, so a bounded id set is
	// visibly a lower bound rather than an exact total.
	Filter *RequirementFilterView `json:"filter,omitempty"`
}

// RequirementFilterView is the repository_id filter, reported back so the
// caller can tell an empty page caused by an unknown Repository from an empty
// page caused by an exhausted cursor, and so the port's own bound is visible.
type RequirementFilterView struct {
	RepositoryID string `json:"repository_id"`
	// LinkedIDsRead is how many Requirement ids the write-once
	// Requirement-to-Repository link read returned for this Repository, before
	// the cursor and the page size were applied.
	LinkedIDsRead int `json:"linked_ids_read"`
	// LinkedIDsBounded is the bool RequirementIDsForRepository itself returns:
	// true means the storage query's own bound truncated the id set, so
	// LinkedIDsRead is a LOWER BOUND on the Repository's linked Requirements
	// and not an exact count.
	LinkedIDsBounded bool `json:"linked_ids_bounded"`
	// Bound is the limit the port was called with.
	Bound int `json:"bound"`
	// Reason states what the two numbers above mean, in the idiom the owner
	// console already uses for a bounded read.
	Reason string `json:"reason"`
	// MissingRequirements counts link rows whose Requirement could not be read
	// back inside the same transaction. It is reported rather than hidden: a
	// link naming a Requirement that is gone is a real, reportable
	// inconsistency, and dropping it silently would make the page size lie.
	MissingRequirements int `json:"missing_requirements"`
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
	// NeedsInput is the question the Loop asked about this Requirement
	// (V2-065), read from the needs-input side table. It follows
	// requestedByView's precedent exactly: a pointer, omitted entirely when
	// no row exists, and never synthesised. A Requirement whose status is
	// needs-input but which has no recorded row reads with this field
	// ABSENT -- that combination is a real, reportable inconsistency, and
	// inventing question text to fill the field would be a fabricated
	// observation. Absent means absent.
	NeedsInput *HumanInputRequest `json:"needs_input,omitempty"`
	// ResumesTo is the status a resume would restore this Requirement to
	// (V2-090), read from domain.Requirement.PausedFrom. It is OMITTED
	// ENTIRELY unless the status is paused, the same discipline repository_id
	// and captured_at already use: absent means absent, and no status is ever
	// synthesised for a Requirement that is not paused.
	//
	// It is on the detail read model rather than only on the pause response
	// because a way out visible only in the answer to the pause is still a trap
	// for anyone who closed the tab. docs/architecture/domain-model.md:269
	// defines the exit as 直前の安全な非終端状態 -- the status the Requirement
	// was actually in -- so an owner looking at a paused Requirement cannot
	// derive it from the status alone; only the record knows.
	//
	// A paused Requirement whose PausedFrom is empty -- one stored before the
	// field existed -- reads with this field ABSENT, which is the honest
	// report: that record has no origin to restore and is cancel-only.
	ResumesTo domain.RequirementStatus `json:"resumes_to,omitempty"`
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
	// Increments is the per-row Increment STATUS as well as its id (V2-095 A6).
	// increment_ids above is unchanged in name, type and meaning and keeps
	// carrying every id the aggregate holds; this field carries the id AND the
	// status of each Increment that was actually read, which is what
	// cap-backlog-visibility's declared confirmation item
	// "進行中Incrementと進捗" asks for. A row whose Increments were bound by
	// the page budget reports IncrementsTruncated, so a short list is never
	// read as a complete one.
	Increments []RequirementRowIncrementView `json:"increments"`
	// IncrementsTruncated reports that THIS row's Increments were cut by the
	// page-wide budget, so both increments and next_action below describe a
	// bounded set. RequirementPage.Truncated is the same fact at page scope.
	IncrementsTruncated bool `json:"increments_truncated"`
	// NextAction is the declared confirmation item "次のaction", computed by
	// the SAME nextAction function the detail view uses, called with the same
	// three arguments: this Requirement's status, the Increments read for it,
	// and the Executions read for those Increments. There is no list-specific
	// variant of that function, because two variants would drift and the drift
	// would show as a Backlog row advising something the detail view does not.
	NextAction string `json:"next_action"`
	// ReleaseReflection is the declared confirmation item
	// "Preview/Stable反映状況", projected from
	// domain.Requirement.StableSnapshot -- a field of the aggregate this page
	// ALREADY reads, so the projection costs zero additional reads. A zero
	// snapshot is reported as an explicit ABSENCE with its reason and never as
	// a plausible release: an owner reading a Backlog must be able to tell
	// "this Requirement is in no release" from "we did not look".
	ReleaseReflection RequirementReleaseReflectionView `json:"release_reflection"`
}

// RequirementRowIncrementView is one Increment as a Backlog row reports it:
// the id and the status, and nothing else. It is deliberately NOT
// RequirementIncrementView: that type carries a Version and an Executions
// list, and putting a per-row Execution list on a page of up to a hundred rows
// is how a bounded owner read becomes an unbounded one.
type RequirementRowIncrementView struct {
	IncrementID string                 `json:"increment_id"`
	Status      domain.IncrementStatus `json:"status"`
}

// RequirementReleaseReflectionView is the Preview/Stable reflection of one
// Requirement, read from domain.Requirement.StableSnapshot.
//
// Observed is false when the recorded snapshot is the zero value, and then
// every identifier field is omitted and Reason says why. This follows
// internal/application/repository.go's ObservedState idiom and the same
// discipline requested_by, captured_at and resumes_to already use on this
// package's read models: absent means absent, and nothing is synthesised.
type RequirementReleaseReflectionView struct {
	Observed       bool           `json:"observed"`
	Reason         string         `json:"reason"`
	ReleaseID      string         `json:"release_id,omitempty"`
	ReleaseVersion domain.Version `json:"release_version,omitempty"`
	BundleDigest   string         `json:"bundle_digest,omitempty"`
	EvidenceDigest string         `json:"evidence_digest,omitempty"`
}

// releaseReflectionAbsent and releaseReflectionObserved are the only two
// answers this projection can give, written once so the absent reason cannot
// drift between call sites.
const releaseReflectionAbsentReason = "no Stable release snapshot is recorded on this Requirement, so it is reflected in no Preview or Stable release; this is a recorded absence and not a failure to look"
const releaseReflectionObservedReason = "the Requirement's own recorded Stable release snapshot, read from canonical state with no additional read"

// releaseReflectionView projects domain.StableReleaseSnapshot onto the wire
// shape. Its single source is the Requirement record handed to it: no store
// read, no clock read and no fallback to a release the process happens to
// know about.
func releaseReflectionView(snapshot domain.StableReleaseSnapshot) RequirementReleaseReflectionView {
	if snapshot == (domain.StableReleaseSnapshot{}) {
		return RequirementReleaseReflectionView{Observed: false, Reason: releaseReflectionAbsentReason}
	}
	return RequirementReleaseReflectionView{
		Observed:       true,
		Reason:         releaseReflectionObservedReason,
		ReleaseID:      snapshot.ReleaseID.String(),
		ReleaseVersion: snapshot.ReleaseVersion,
		BundleDigest:   snapshot.BundleDigest,
		EvidenceDigest: snapshot.EvidenceDigest,
	}
}

// boundPageSize authors the page_size refusal, so the V2-083 caller-fault
// marker goes here. The message is unchanged: a caller still reads
// "page_size must be between 1 and 100".
func boundPageSize(size int) (int, error) {
	if size == 0 {
		return 25, nil
	}
	if size < 0 || size > MaxPageSize {
		return 0, invalidRequest(fmt.Errorf("page_size must be between 1 and %d", MaxPageSize))
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
// humanInputView mirrors requestedByView for the needs-input question
// (V2-065). Its single source is the side-table read handed to it: there is
// no fallback that looks at the Requirement's status, so a needs-input
// Requirement with no row yields nil and the response omits needs_input
// entirely rather than reporting an empty question. A deep COPY is returned,
// so a caller cannot mutate stored state through the pointer or through the
// record's option and scope slices.
func humanInputView(v HumanInputRequest, ok bool) *HumanInputRequest {
	if !ok {
		return nil
	}
	out := v.Clone()
	return &out
}

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
//
// incrementBudget is the V2-095 A6 per-page Increment plan: which Increment ids
// were actually read for each row, and whether that row was cut by the
// page-wide cap. It is computed BEFORE this function by planPageIncrements and
// handed in, so this projection stays a pure function of values already read
// and performs no read of its own.
func requirementViews(rows []domain.Requirement, texts map[string]string, links map[string]domain.RequirementRepositoryLink, plan pageIncrementPlan, incs map[string]domain.Increment, execsByIncrement map[string][]domain.Execution) []RequirementView {
	out := make([]RequirementView, 0, len(rows))
	for _, r := range rows {
		id := r.ID.String()
		ids := make([]string, len(r.Increments))
		for i, incID := range r.Increments {
			ids[i] = incID.String()
		}
		// The two arguments nextAction takes besides the status, assembled for
		// exactly this row out of the two batch reads. The SAME function the
		// detail view calls is called with the SAME kinds of argument; there is
		// no list-specific variant.
		rowIncrements := make([]RequirementRowIncrementView, 0, len(plan.readFor[id]))
		rowIncs := make([]domain.Increment, 0, len(plan.readFor[id]))
		rowExecs := []domain.Execution{}
		for _, incID := range plan.readFor[id] {
			inc, ok := incs[incID]
			if !ok {
				// A Requirement naming an Increment the batch read did not
				// return is reported by OMITTING that Increment rather than by
				// inventing a status for it. increment_ids above still carries
				// the id, so the discrepancy stays visible.
				continue
			}
			rowIncrements = append(rowIncrements, RequirementRowIncrementView{IncrementID: inc.ID.String(), Status: inc.Status})
			rowIncs = append(rowIncs, inc)
			rowExecs = append(rowExecs, execsByIncrement[incID]...)
		}
		out = append(out, RequirementView{
			RequirementID:       id,
			Status:              r.Status,
			Version:             r.Version,
			IncrementIDs:        ids,
			Text:                texts[id],
			RequestedBy:         requestedByView(r.RequestedBy),
			RepositoryID:        links[id].RepositoryID.String(),
			CapturedAt:          capturedAtView(r),
			Increments:          rowIncrements,
			IncrementsTruncated: plan.truncatedFor[id],
			NextAction:          nextAction(r.Status, rowIncs, rowExecs),
			ReleaseReflection:   releaseReflectionView(r.StableSnapshot),
		})
	}
	return out
}

// pageIncrementPlan is which Increment ids the page will read, per row, and
// which rows the page-wide cap cut.
//
// THE CAP IS ACROSS THE PAGE, NOT PER ROW. A page of a hundred rows each
// holding a hundred Increments would otherwise turn one bounded owner read
// into a ten-thousand-id batch read. The total is capped at MaxPageSize ids;
// rows are filled in page order until the budget is exhausted, and every row
// that lost ids -- including a row that got none at all -- is marked, so a
// short list is never readable as a complete one.
type pageIncrementPlan struct {
	all          []string
	readFor      map[string][]string
	truncatedFor map[string]bool
	truncated    bool
}

func planPageIncrements(rows []domain.Requirement, budget int) pageIncrementPlan {
	plan := pageIncrementPlan{readFor: map[string][]string{}, truncatedFor: map[string]bool{}}
	remaining := budget
	if remaining < 0 {
		remaining = 0
	}
	for _, r := range rows {
		id := r.ID.String()
		taken := make([]string, 0, len(r.Increments))
		for _, incID := range r.Increments {
			if remaining == 0 {
				plan.truncatedFor[id] = true
				plan.truncated = true
				break
			}
			taken = append(taken, incID.String())
			remaining--
		}
		plan.readFor[id] = taken
		plan.all = append(plan.all, taken...)
	}
	return plan
}

// readPageIncrements performs the EXACTLY TWO new batch reads this page makes:
// one IncrementsForRequirements over the planned id set and one
// ExecutionsForIncrements over the Increment ids that read actually returned.
// Both are called ONCE per page and never once per row, and both are skipped
// entirely when the plan is empty, so a Backlog of Requirements with no
// Increments costs zero extra reads.
func readPageIncrements(ctx context.Context, u UnitOfWork, plan pageIncrementPlan) (map[string]domain.Increment, map[string][]domain.Execution, error) {
	byID := map[string]domain.Increment{}
	byIncrement := map[string][]domain.Execution{}
	if len(plan.all) == 0 {
		return byID, byIncrement, nil
	}
	incs, err := u.IncrementsForRequirements(ctx, plan.all)
	if err != nil {
		return nil, nil, err
	}
	readIDs := make([]string, 0, len(incs))
	for _, inc := range incs {
		byID[inc.ID.String()] = inc
		readIDs = append(readIDs, inc.ID.String())
	}
	if len(readIDs) == 0 {
		return byID, byIncrement, nil
	}
	execs, err := u.ExecutionsForIncrements(ctx, readIDs)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range execs {
		key := e.IncrementID.String()
		byIncrement[key] = append(byIncrement[key], e)
	}
	return byID, byIncrement, nil
}

// MaxRepositoryFilterID bounds the repository_id query parameter. A repository
// id is an opaque identifier (domain.NewRepositoryID refuses only a blank one),
// so the shape refusal here is length and encoding, in the same style
// decodeCursor already bounds its own decoded key at 512 bytes.
const MaxRepositoryFilterID = 512

// ErrInvalidRepositoryFilter is the shape refusal for the repository_id query
// parameter. It is wrapped as a caller fault, so the route answers 400
// invalid_request with the existing message style rather than 500.
var ErrInvalidRepositoryFilter = fmt.Errorf("repository_id must be a non-blank identifier of at most %d bytes", MaxRepositoryFilterID)

// ListRequirementsPage is the unfiltered Backlog page. Its signature is
// UNCHANGED: every existing caller, including the ones in prohibited packages,
// keeps compiling and keeps meaning the same thing.
func (s *Service) ListRequirementsPage(ctx context.Context, cursor string, pageSize int) (RequirementPage, error) {
	return s.ListRequirementsPageFiltered(ctx, cursor, pageSize, "")
}

// ListRequirementsPageFiltered is the Backlog page with the optional
// repository_id filter escalation E22-7 measured absent (V2-095 A8).
//
// repositoryID == "" is the unfiltered page and behaves exactly as before: one
// bounded RequirementsPage read ordered by the adapter's own key.
//
// A non-empty repositoryID resolves through the EXISTING
// RequirementRepositoryLinkRepository.RequirementIDsForRepository port and
// through nothing else. No new port, no new storage query and no new index is
// introduced; internal/store/** is byte-unchanged, which is the proof of that.
// An UNKNOWN repository id therefore yields an EMPTY id set and an EMPTY page,
// never the unfiltered page -- which is the exact defect E22-7 measured, where
// the parameter was ignored and the caller was handed every Requirement.
//
// ORDERING, stated because the two modes differ and a reader must not assume
// they do not. The unfiltered page is ordered by the adapter's own document
// key; the filtered page is ordered by Requirement id, which is the order both
// adapters' RequirementIDsForRepository already returns. The cursor value is a
// Requirement id in both modes, so a cursor always means "resume after this
// Requirement id"; it is not meaningful to carry a cursor from one mode into
// the other, and neither mode can be made to skip or repeat a row by doing so,
// because each resumes from a position in its own order.
func (s *Service) ListRequirementsPageFiltered(ctx context.Context, cursor string, pageSize int, repositoryID string) (RequirementPage, error) {
	if _, _, err := callerActor(ctx, RoleOwner); err != nil {
		return RequirementPage{}, err
	}
	limit, err := boundPageSize(pageSize)
	if err != nil {
		return RequirementPage{}, err
	}
	filtered := repositoryID != ""
	if filtered {
		if strings.TrimSpace(repositoryID) == "" || len(repositoryID) > MaxRepositoryFilterID || !utf8.ValidString(repositoryID) {
			return RequirementPage{}, invalidRequest(ErrInvalidRepositoryFilter)
		}
	}
	after, err := decodeCursor(cursor)
	if err != nil {
		return RequirementPage{}, err
	}
	var page RequirementPage
	err = s.transact(ctx, func(u UnitOfWork) error {
		var rows []domain.Requirement
		var more bool
		if filtered {
			rows, more, err = repositoryFilteredRows(ctx, u, repositoryID, after, limit, &page)
		} else {
			rows, more, err = u.RequirementsPage(ctx, after, limit)
		}
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
		plan := planPageIncrements(rows, MaxPageSize)
		incs, execsByIncrement, e := readPageIncrements(ctx, u, plan)
		if e != nil {
			return e
		}
		page.Requirements = requirementViews(rows, texts, links, plan, incs, execsByIncrement)
		page.PageSize = limit
		page.Truncated = plan.truncated
		if more && len(rows) != 0 {
			page.NextCursor = encodeCursor(rows[len(rows)-1].ID.String())
		}
		return nil
	})
	if err != nil {
		return RequirementPage{}, err
	}
	return page, nil
}

// repositoryFilteredRows is the filtered read: ONE bounded link query through
// the existing port, then at most limit keyed Requirement reads for the ids
// that survive the cursor. It writes the filter report onto page as it goes,
// including the port's own truncation bool, so the bound is surfaced rather
// than hidden.
//
// A link row naming a Requirement that cannot be read back is COUNTED and
// reported, not silently dropped: dropping it would make the page shorter than
// the page size for no visible reason.
func repositoryFilteredRows(ctx context.Context, u UnitOfWork, repositoryID, after string, limit int, page *RequirementPage) ([]domain.Requirement, bool, error) {
	linked, bounded, err := u.RequirementIDsForRepository(ctx, repositoryID, MaxPageSize)
	if err != nil {
		return nil, false, err
	}
	sorted := append([]string(nil), linked...)
	sort.Strings(sorted)
	filter := &RequirementFilterView{
		RepositoryID:     repositoryID,
		LinkedIDsRead:    len(sorted),
		LinkedIDsBounded: bounded,
		Bound:            MaxPageSize,
	}
	if bounded {
		filter.Reason = fmt.Sprintf("the write-once Requirement-to-Repository link query applied its own bound of %d, so linked_ids_read is a LOWER BOUND on this Repository's linked Requirements and not an exact total", MaxPageSize)
	} else {
		filter.Reason = fmt.Sprintf("the write-once Requirement-to-Repository link query returned %d linked Requirement ids within its bound of %d, so this is the whole set", len(sorted), MaxPageSize)
	}
	page.Filter = filter

	window := make([]string, 0, len(sorted))
	for _, id := range sorted {
		if after != "" && id <= after {
			continue
		}
		window = append(window, id)
	}
	more := len(window) > limit
	if more {
		window = window[:limit]
	}
	rows := make([]domain.Requirement, 0, len(window))
	for _, id := range window {
		r, found, e := u.Requirement(ctx, id)
		if e != nil {
			return nil, false, e
		}
		if !found {
			filter.MissingRequirements++
			continue
		}
		rows = append(rows, r)
	}
	return rows, more, nil
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
		question, hasQuestion, err := u.HumanInputRequest(ctx, id)
		if err != nil {
			return err
		}
		out.NeedsInput = humanInputView(question, hasQuestion)
		// V2-090: the exit, made visible on the read surface and not only in
		// the response to the pause. Gated on the status rather than on the
		// field being non-empty, so a stale memory on a non-paused record could
		// never be reported as a resumption target.
		if r.Status == domain.RequirementPaused {
			out.ResumesTo = r.PausedFrom
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
		// Authored here, marked here (V2-083). The message is unchanged.
		return nil, invalidRequest(fmt.Errorf("page_size must be at most %d", MaxPageSize))
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

// QueueSummary is the bounded counter read model. Its five fields keep their
// names, types and meanings: it is the value QueueSummaryRepository returns and
// it is also published as RepositoryBacklogView.InstallationScope, so its shape
// is a contract two responses share.
//
// The allocation, waiting and exhaustion objects GET /v1/queue/summary adds
// (V2-068) live on QueueSummaryResponse in allocation.go, which embeds this
// type, rather than on this type: see the recorded reason there.
type QueueSummary struct {
	Requirements        int            `json:"requirements"`
	ByRequirementStatus map[string]int `json:"by_requirement_status"`
	Increments          int            `json:"increments"`
	ByIncrementStatus   map[string]int `json:"by_increment_status"`
	ActiveExecutions    int            `json:"active_executions"`
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
		// Authored here, marked here (V2-083). The transport's own
		// invalid_limit check on GET /v1/export refuses an out-of-range limit
		// before this one is reached, so no pin-table row exercises it; it is
		// marked anyway because it is the same class of refusal and
		// errors_test.go asserts it directly.
		return nil, invalidRequest(fmt.Errorf("export limit must be between 1 and %d", MaxPageSize))
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

// ===========================================================================
// The owner-readable documentation surface (V2-095 A9, escalation E22-10)
// ===========================================================================

// ErrReleaseDocumentNotFound is returned when the requested path is not a
// member of the assembled documentation-role set. It is distinguishable on
// purpose: the route maps exactly this error to 404 with an explicit code, and
// it is returned BEFORE any file is opened.
var ErrReleaseDocumentNotFound = errors.New("no such document in the assembled documentation set for the channel and version in use")

// ErrReleaseDocumentDrifted is returned when a member's bytes on disk no
// longer hash to the digest the assembled bundle recorded. It fails closed
// rather than serving the drifted bytes: cap-user-documentation's declared
// success condition is that the documents correspond to the channel and
// version IN USE, and bytes that disagree with the recorded manifest
// correspond to no version at all.
var ErrReleaseDocumentDrifted = errors.New("the document's bytes no longer match the digest the assembled release bundle recorded for it")

// ReleaseDocumentIndexView is the answer to the owner document index. It names
// the channel the release package resolved, the version of the assembled
// contract, and the documents available -- which is exactly the assembled
// bundle's documentation-role member set and nothing else.
type ReleaseDocumentIndexView struct {
	SchemaVersion string `json:"schema_version"`
	// Channel is release.ResolveChannel's answer: "stable" when this process
	// has recorded a Stable route, "preview" otherwise. It is not a literal.
	Channel string `json:"channel"`
	// ReleaseVersion is the version of the assembled contract, read from the
	// SAME observer GET /v1/release/state reads, so the two answers cannot
	// disagree about which version the documents describe.
	ReleaseVersion string `json:"release_version"`
	// DocsDigest is the documentation role's own digest, so a reader can tell
	// two document sets apart without fetching them.
	DocsDigest string `json:"docs_digest"`
	// AllowlistSource states, in prose, that the served set is DERIVED and not
	// chosen, and how.
	AllowlistSource string                     `json:"allowlist_source"`
	Documents       []ReleaseDocumentEntryView `json:"documents"`
}

// ReleaseDocumentEntryView is one available document: the repository-relative
// path the bundle recorded, its recorded digest, and the route that serves it.
type ReleaseDocumentEntryView struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Route  string `json:"route"`
}

// ReleaseDocumentView is one served document.
type ReleaseDocumentView struct {
	Path           string `json:"path"`
	Channel        string `json:"channel"`
	ReleaseVersion string `json:"release_version"`
	SHA256         string `json:"sha256"`
	SizeBytes      int    `json:"size_bytes"`
	Content        string `json:"content"`
}

// ReleaseDocumentRoutePrefix is the owner route prefix the per-document route
// hangs off. It is declared here, next to the projection that builds the Route
// field, so the route string a caller is handed and the route string the
// transport parses come from ONE definition.
const ReleaseDocumentRoutePrefix = "/owner/"

// releaseDocumentAllowlistSource is fixed prose, not a measurement: it states
// where the served set came from, so a reader is never left to assume the
// handler joins a caller-supplied path onto a root.
const releaseDocumentAllowlistSource = "the documentation-role member set of the release bundle this process assembled from its explicitly configured source root (internal/release's RoleDocumentation, globs docs/preview/**.md and docs/stable/**.md). The set is recomputed from the tree at attachment, never written down here, and a request is answered by SET MEMBERSHIP on the recorded member path: a path outside the set is refused before any file is opened, so there is no path to traverse."

// releaseDocumentSet is the assembled documentation-role members keyed by the
// path a caller names, plus the channel and version the same observer reports.
//
// IT IS THE ONE PLACE the two document routes resolve anything, so the index
// and the per-document route can never serve different sets. Both answer the
// SAME not-configured error GET /v1/release/state answers, because both read
// the same binding: with no explicitly configured release source root this
// process can report no channel, no version and no document set, and a
// defaulted root would make it report a version it was not assembled from
// (internal/application/release_surface.go's own reason).
func (s *Service) releaseDocumentSet() (members map[string]release.Member, order []string, channel, version, docsDigest, root string, err error) {
	source, configured := s.releaseSource()
	observer, attached := s.releaseObserver()
	if !configured || !attached {
		return nil, nil, "", "", "", "", ErrReleaseObserverNotConfigured
	}
	report, _ := observer.ReleaseSnapshot()
	route, recorded := observer.RecordedRoute()
	stableExists := recorded && route.StableDigest != ""
	members = map[string]release.Member{}
	for _, m := range source.bundle.Members {
		if m.Role != release.RoleDocumentation {
			continue
		}
		if existing, clash := members[m.Path]; clash {
			// Unreachable while assembleMembers yields one member per path.
			// Failing closed here rather than picking one keeps a duplicate
			// visible instead of invisible.
			return nil, nil, "", "", "", "", fmt.Errorf("the assembled documentation set names %s twice (%s and %s)", m.Path, existing.SHA256, m.SHA256)
		}
		members[m.Path] = m
		order = append(order, m.Path)
	}
	if len(members) == 0 {
		return nil, nil, "", "", "", "", fmt.Errorf("the assembled release bundle carries no documentation-role member; there is no document set to serve")
	}
	sort.Strings(order)
	return members, order, release.ResolveChannel(stableExists), report.ReleaseVersion, report.DocsDigest, source.pipeline.Root, nil
}

// ReleaseDocumentIndex is the owner read behind GET /owner/docs/. It is gated
// exactly as the other owner reads are and opens no file at all: every value
// it reports was computed when the release source was attached.
func (s *Service) ReleaseDocumentIndex(ctx context.Context) (ReleaseDocumentIndexView, error) {
	if _, _, err := callerActor(ctx, RoleOwner); err != nil {
		return ReleaseDocumentIndexView{}, err
	}
	members, order, channel, version, docsDigest, _, err := s.releaseDocumentSet()
	if err != nil {
		return ReleaseDocumentIndexView{}, err
	}
	out := ReleaseDocumentIndexView{
		SchemaVersion:   "v1",
		Channel:         channel,
		ReleaseVersion:  version,
		DocsDigest:      docsDigest,
		AllowlistSource: releaseDocumentAllowlistSource,
		Documents:       make([]ReleaseDocumentEntryView, 0, len(order)),
	}
	for _, path := range order {
		m := members[path]
		out.Documents = append(out.Documents, ReleaseDocumentEntryView{
			Path:   m.Path,
			SHA256: m.SHA256,
			Route:  ReleaseDocumentRoutePrefix + m.Path,
		})
	}
	return out, nil
}

// ReleaseDocument is the owner read behind GET /owner/{member-path}. path is
// the caller's string and is used for EXACTLY ONE thing: a lookup in the
// assembled member map. It is never joined onto a root, never cleaned, never
// resolved and never passed to the filesystem. The path that IS opened is the
// member's OWN recorded path, so a traversal attempt, an absolute path, a
// symlink target and a URL-encoded escape are all simply absent from the map
// and refused before any file is opened.
func (s *Service) ReleaseDocument(ctx context.Context, path string) (ReleaseDocumentView, error) {
	if _, _, err := callerActor(ctx, RoleOwner); err != nil {
		return ReleaseDocumentView{}, err
	}
	members, _, channel, version, _, root, err := s.releaseDocumentSet()
	if err != nil {
		return ReleaseDocumentView{}, err
	}
	member, ok := members[path]
	if !ok {
		return ReleaseDocumentView{}, ErrReleaseDocumentNotFound
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(member.Path)))
	if err != nil {
		return ReleaseDocumentView{}, err
	}
	digest := sha256.Sum256(data)
	served := hex.EncodeToString(digest[:])
	if served != member.SHA256 {
		return ReleaseDocumentView{}, fmt.Errorf("%w: %s", ErrReleaseDocumentDrifted, member.Path)
	}
	return ReleaseDocumentView{
		Path:           member.Path,
		Channel:        channel,
		ReleaseVersion: version,
		SHA256:         served,
		SizeBytes:      len(data),
		Content:        string(data),
	}, nil
}
