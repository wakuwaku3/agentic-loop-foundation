// Package memory provides a small transactional adapter used by application
// tests and by the M0/M1 vertical slice. It intentionally has no domain
// policy: policy remains in application and domain packages.
package memory

import (
	"context"
	"errors"
	"hash/crc32"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/quota"
)

// ErrOptimisticConflict is an adapter spelling of the domain's stale-version
// condition. Callers may use either errors.Is value at the boundary.
var ErrOptimisticConflict = domain.ErrStaleVersion

type state struct {
	requirements       map[string]domain.Requirement
	increments         map[string]domain.Increment
	executions         map[string]domain.Execution
	leases             map[string]domain.Lease
	controls           []domain.ControlIntent
	controlProgress    map[domain.Revision]domain.ControlProgress
	controlRequestedBy map[domain.Revision]domain.RequestedBy
	allocationLimits   map[domain.Revision]application.AllocationLimit
	runnerObservations map[string]domain.RunnerObservation
	runnerVersions     map[string]application.RunnerVersionReport
	providerLogs       map[application.ProviderName]application.ProviderObservationLog
	providerAssigns    map[string]application.ProviderAssignment
	providerAssignSeq  map[application.ProviderName][]application.ProviderAssignment
	repositories       map[string]domain.Repository
	repositoryObs      map[string]domain.RepositoryObservation
	requirementRepo    map[string]domain.RequirementRepositoryLink
	publications       map[string]domain.PublicationObservation
	humanInput         map[string]application.HumanInputRequest
	requests           map[string]application.IdempotentResponse
	texts              map[string]string
	targets            map[string]domain.ControlTarget
	events             []application.Event
	outbox             []application.OutboxItem
	quota              quota.Counter
}

func newState() state {
	return state{requirements: map[string]domain.Requirement{}, increments: map[string]domain.Increment{}, executions: map[string]domain.Execution{}, leases: map[string]domain.Lease{}, requests: map[string]application.IdempotentResponse{}, texts: map[string]string{}, targets: map[string]domain.ControlTarget{}, controlProgress: map[domain.Revision]domain.ControlProgress{}, controlRequestedBy: map[domain.Revision]domain.RequestedBy{}, allocationLimits: map[domain.Revision]application.AllocationLimit{}, runnerObservations: map[string]domain.RunnerObservation{}, runnerVersions: map[string]application.RunnerVersionReport{}, providerLogs: map[application.ProviderName]application.ProviderObservationLog{}, providerAssigns: map[string]application.ProviderAssignment{}, providerAssignSeq: map[application.ProviderName][]application.ProviderAssignment{}, repositories: map[string]domain.Repository{}, repositoryObs: map[string]domain.RepositoryObservation{}, requirementRepo: map[string]domain.RequirementRepositoryLink{}, publications: map[string]domain.PublicationObservation{}, humanInput: map[string]application.HumanInputRequest{}}
}
func (s state) clone() state {
	n := newState()
	for k, v := range s.requirements {
		v.Increments = append([]domain.IncrementID(nil), v.Increments...)
		n.requirements[k] = v
	}
	for k, v := range s.increments {
		n.increments[k] = v
	}
	for k, v := range s.executions {
		n.executions[k] = v
	}
	for k, v := range s.leases {
		n.leases[k] = v
	}
	n.controls = append([]domain.ControlIntent(nil), s.controls...)
	for k, v := range s.controlProgress {
		n.controlProgress[k] = v
	}
	for k, v := range s.controlRequestedBy {
		n.controlRequestedBy[k] = v
	}
	// The installation concurrency limit side table is copied on write like
	// every other record, so a rolled-back transaction cannot leak a limit
	// into the committed state.
	for k, v := range s.allocationLimits {
		n.allocationLimits[k] = v
	}
	// The needs-input question row is deep-copied (V2-065): its option and
	// scope slices must not be shared with committed state, or a rolled-back
	// transaction could still have rewritten a recorded question in place.
	for k, v := range s.humanInput {
		n.humanInput[k] = v.Clone()
	}
	for k, v := range s.runnerObservations {
		v.Processes = append([]domain.ProcessObservation(nil), v.Processes...)
		n.runnerObservations[k] = v
	}
	// The Runner version report is copied on write like every other record,
	// so a rolled-back transaction cannot leak a report into committed state.
	for k, v := range s.runnerVersions {
		n.runnerVersions[k] = v
	}
	// The Provider observation ring and the Provider assignment side table are
	// copied on write like every other record, including their slices, so a
	// rolled-back transaction cannot leak an observation or an assignment into
	// the committed state.
	for k, v := range s.providerLogs {
		v.Observations = append([]application.ProviderObservation(nil), v.Observations...)
		n.providerLogs[k] = v
	}
	for k, v := range s.providerAssigns {
		n.providerAssigns[k] = v
	}
	for k, v := range s.providerAssignSeq {
		n.providerAssignSeq[k] = append([]application.ProviderAssignment(nil), v...)
	}
	// Repository and its bounded forge Observation are copied on write like
	// every other aggregate, so a rolled-back transaction cannot leak a
	// registration or an observation into the committed state.
	for k, v := range s.repositories {
		n.repositories[k] = v
	}
	for k, v := range s.repositoryObs {
		n.repositoryObs[k] = v
	}
	// The Requirement-to-Repository link is copied on write like every other
	// record, so a rolled-back transaction cannot leak an association into
	// the committed state.
	for k, v := range s.requirementRepo {
		n.requirementRepo[k] = v
	}
	// The publication Observation is copied on write like every other record,
	// so a rolled-back transaction cannot leak an Observation of an external
	// effect that never happened into the committed state.
	for k, v := range s.publications {
		n.publications[k] = v
	}
	for k, v := range s.requests {
		n.requests[k] = v
	}
	for k, v := range s.texts {
		n.texts[k] = v
	}
	for k, v := range s.targets {
		n.targets[k] = v
	}
	n.events = append([]application.Event(nil), s.events...)
	n.quota = s.quota
	for _, v := range s.outbox {
		v.Payload = append([]byte(nil), v.Payload...)
		n.outbox = append(n.outbox, v)
	}
	return n
}

type Store struct {
	mu   sync.Mutex
	data state
}

func New() *Store { return &Store{data: newState()} }

// NewStore is the descriptive spelling used by adapters and tests.
func NewStore() *Store { return New() }

type Clock struct{ NowValue time.Time }

func (c Clock) Now() time.Time { return c.NowValue }

// unit's quota accounting fields mirror internal/store/firestore's unit one
// for one: reads is that adapter's u.cache (every distinct document this
// transaction fetched), writes is its u.values (every distinct document it
// staged), and quotaKey/quotaReserved/quotaHeld are its u.quotaKey,
// u.quotaReserved and u.quotaRef.
type unit struct {
	s             *state
	ctx           context.Context
	reads         map[string]bool
	writes        map[string]bool
	quotaKey      string
	quotaReserved quota.Usage
	quotaHeld     bool
}

// ===========================================================================
// Quota accounting (V2-087)
// ===========================================================================
//
// internal/application/service.go reserves the worst case BEFORE the callback
// can stage anything -- quota.ReadTransactionUsage (6,001 reads, 1 write) at
// service.go:136 for a read transaction and quota.MutationUsage at
// service.go:153 for a mutation -- and internal/store/firestore then SETTLES
// that reservation: its trueUpQuota (firestore/store.go:481-497) computes
// {Reads: len(u.cache), Writes: len(u.values)} and calls quota.Counter.TrueUp
// before flush() issues any write (firestore/store.go:566-568), while
// countReads (firestore/store.go:519-525) and read (firestore/store.go:499
// -513) feed that cache from every query, batch get and keyed read.
//
// Until V2-087 this adapter's entire quota implementation was the single
// Reserve call in ReserveQuota below and nothing ever settled it, so 25,000 /
// 6,001 meant exactly FOUR read transactions fitted in a UTC day here while
// effectively unlimited ones fitted on Firestore -- and a green test on this
// adapter was therefore not evidence about the other one.
//
// What is counted is the set of distinct RECORD KEYS a transaction touched,
// mirroring the Firestore DOCUMENT set rather than rows returned or calls
// made (dp-v2-087 d2):
//
//	(a) a key counts once however many times it is read, mirroring the fact
//	    that u.cache is keyed by document path;
//	(b) a MISS counts as one read, because firestore/store.go:499-513 caches
//	    nil for a NotFound and Firestore bills a get of a missing document;
//	(c) a paged read counts min(limit+1, rows remaining after the cursor),
//	    because the Firestore query is Limit(limit+1) and countReads sees
//	    every snapshot it returned (firestore/store.go:657-662);
//	(d) the quota record counts as one read and one write, because the
//	    Firestore adapter reads it through u.value and stages it;
//	(e) a write counts its key once, mirroring len(u.values);
//	(f) a single-key read of a key already written in this transaction counts
//	    nothing, mirroring u.value's short-circuit on u.values
//	    (firestore/store.go:527-533). countFetchedReads is the helper for the
//	    paths where Firestore reaches the server anyway -- read() and every
//	    query -- and it deliberately does NOT short-circuit;
//	(g) deletes stay zero, because neither adapter has a delete operation.
//
// Keys are namespaced by KIND, so two aggregates that share an id are two
// records here exactly as they are two document paths in Firestore. The kind
// names are the Firestore collection names they mirror.
const (
	kindQuota                      = "quota"
	kindRequirements               = "requirements"
	kindIncrements                 = "increments"
	kindExecutions                 = "executions"
	kindLeases                     = "leases"
	kindControls                   = "controls"
	kindControlProgress            = "control_progress"
	kindControlRequestedBy         = "control_requested_by"
	kindAllocationLimits           = "allocation_limits"
	kindTargets                    = "targets"
	kindRunnerObservations         = "runner_observations"
	kindRunnerVersionReports       = "runner_version_reports"
	kindProviderRegistry           = "provider_registry"
	kindProviderAssignments        = "provider_assignments"
	kindRepositories               = "repositories"
	kindRepositoryObservations     = "repository_observations"
	kindRequirementRepositoryLinks = "requirement_repository_links"
	kindRequirementNeedsInput      = "requirement_needs_input"
	kindPublicationObservations    = "publication_observations"
	kindIdempotency                = "idempotency"
	kindEvents                     = "events"
	kindOutbox                     = "outbox"
	kindQueueCounters              = "queue_counters"
)

// queueShards mirrors internal/store/firestore's queueShards.
const queueShards = 32

// keySeparator joins a kind to a record id. It is not a Firestore path: it
// only has to make two kinds' id spaces disjoint.
const keySeparator = "/"

func countedKey(kind, id string) string { return kind + keySeparator + id }

// revisionKey is the document id the Firestore adapter gives a revision-keyed
// record (fmt.Sprintf("%d", revision) at firestore/store.go).
func revisionKey(revision domain.Revision) string { return strconv.FormatInt(int64(revision), 10) }

// shardKey is the document id the Firestore adapter gives a queue-counter
// shard (fmt.Sprintf("%02d", shard)).
func shardKey(shard int) string {
	s := strconv.Itoa(shard)
	if len(s) < 2 {
		s = "0" + s
	}
	return s
}

// countKeyedRead charges one keyed read. A key already staged in this
// transaction is not charged, mirroring u.value's short-circuit on u.values:
// the Firestore adapter answers such a read without going to the server.
func (u *unit) countKeyedRead(kind, id string) {
	key := countedKey(kind, id)
	if u.writes[key] {
		return
	}
	u.reads[key] = true
}

// countFetchedReads charges every document a query, a paged read or a batch
// get actually returned, mirroring countReads. These paths reach the server
// even for a key this transaction has already staged, so they never
// short-circuit.
func (u *unit) countFetchedReads(kind string, ids ...string) {
	for _, id := range ids {
		u.reads[countedKey(kind, id)] = true
	}
}

// countWrite charges one staged write, mirroring the single u.values entry
// u.stage keeps per document path.
func (u *unit) countWrite(kind, id string) { u.writes[countedKey(kind, id)] = true }

// countQueueCounter charges the sharded queue-counter document the Firestore
// adapter reads and re-stages on every aggregate status change (adjustCounter,
// firestore/store.go:2091-2119). This adapter derives QueueSummary from its
// maps and keeps no counter record, but the reservation being settled is the
// same one, so the same document has to be paid for here or the two adapters
// would disagree about what a mutation costs.
func (u *unit) countQueueCounter(id string) {
	shard := shardKey(int(crc32.ChecksumIEEE([]byte(id)) % queueShards))
	u.countKeyedRead(kindQueueCounters, shard)
	u.countWrite(kindQueueCounters, shard)
}

// countLeaseScan charges the unfiltered lease-collection scan that the
// Firestore adapter's queryWhere performs for every increment-keyed lease
// lookup: that adapter reads the collection and applies the predicate to the
// decoded values, so it is charged for every lease document rather than for
// the matching one.
func (u *unit) countLeaseScan() {
	for id := range u.s.leases {
		u.countFetchedReads(kindLeases, id)
	}
}

// countControlScan charges the unfiltered Control Intent collection scan that
// the Firestore adapter's u.query performs, keyed by revision exactly as that
// adapter keys the documents.
func (u *unit) countControlScan() {
	for _, v := range u.s.controls {
		u.countFetchedReads(kindControls, revisionKey(v.Revision))
	}
}

// boundedWindow is how many rows a Limit(limit+1) query would have returned
// out of the rows remaining after the cursor: the requested page plus the one
// probe row that tells the caller whether another page exists (clause c).
func boundedWindow(remaining, limit int) int {
	if limit < 0 {
		limit = 0
	}
	if remaining < limit+1 {
		return remaining
	}
	return limit + 1
}

// trueUpQuota settles the worst-case reservation ReserveQuota admitted down to
// the keys this transaction actually read and wrote. It reuses
// quota.Counter.TrueUp -- the same correction the Firestore adapter applies,
// not a second one -- which clamps the actual usage component-wise to
// [0, reserved], so the settled total can neither exceed the worst case
// Reserve already checked nor fall below zero.
func (u *unit) trueUpQuota() {
	if !u.quotaHeld {
		return
	}
	actual := quota.Usage{Reads: int64(len(u.reads)), Writes: int64(len(u.writes))}
	u.s.quota.TrueUp(u.quotaKey, u.quotaReserved, actual)
}

func (u *unit) ReserveQuota(_ context.Context, key string, at time.Time, usage quota.Usage) error {
	// One document on the Firestore side: ReserveQuota reads the day's record
	// through u.value and stages it, so it costs one read and one write there
	// and has to cost the same here (clause d).
	u.countKeyedRead(kindQuota, quota.Day(at))
	if err := u.s.quota.Reserve(at, key, usage, quota.DefaultBudget); err != nil {
		return err
	}
	u.countWrite(kindQuota, quota.Day(at))
	// Remember the worst case so Store.Transact can settle it once the
	// callback has returned. ReserveQuota is called exactly once per
	// transaction, before anything else is staged.
	u.quotaKey = key
	u.quotaReserved = usage
	u.quotaHeld = true
	return nil
}

func (m *Store) Transact(ctx context.Context, fn func(application.UnitOfWork) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	working := m.data.clone()
	u := &unit{s: &working, ctx: ctx, reads: map[string]bool{}, writes: map[string]bool{}}
	if err := fn(u); err != nil {
		return err
	}
	// The true-up runs on the CLONE, after the callback returned nil and
	// BEFORE the swap -- the same position flush() gives it in the Firestore
	// adapter (firestore/store.go:566-568). A failed transaction therefore
	// drops the reservation and its correction together, exactly as an
	// aborted Firestore transaction does.
	u.trueUpQuota()
	m.data = working
	return nil
}

// AuthorityContext is used by application event recording to carry the
// timestamp captured before a transaction callback.
func (u *unit) AuthorityContext() context.Context { return u.ctx }

func (u *unit) Requirement(_ context.Context, id string) (domain.Requirement, bool, error) {
	u.countKeyedRead(kindRequirements, id)
	v, ok := u.s.requirements[id]
	if ok {
		v.Increments = append([]domain.IncrementID(nil), v.Increments...)
	}
	return v, ok, nil
}
func (u *unit) Requirements(_ context.Context) ([]domain.Requirement, error) {
	out := make([]domain.Requirement, 0, len(u.s.requirements))
	for k := range u.s.requirements {
		// The Firestore counterpart is u.query, an unfiltered
		// Limit(MaxQueryRows+1) scan of the collection, so every document is
		// charged.
		u.countFetchedReads(kindRequirements, k)
	}
	for _, v := range u.s.requirements {
		v.Increments = append([]domain.IncrementID(nil), v.Increments...)
		out = append(out, v)
	}
	return out, nil
}

// RequirementsPage keeps pagination work proportional to the requested page;
// the in-memory adapter mirrors Firestore's ordering and exclusive cursor.
func (u *unit) RequirementsPage(_ context.Context, afterID string, limit int) ([]domain.Requirement, bool, error) {
	rows := make([]domain.Requirement, 0, len(u.s.requirements))
	for _, v := range u.s.requirements {
		v.Increments = append([]domain.IncrementID(nil), v.Increments...)
		rows = append(rows, v)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID.String() < rows[j].ID.String() })
	start := 0
	for start < len(rows) && rows[start].ID.String() <= afterID {
		start++
	}
	if limit <= 0 {
		return nil, false, nil
	}
	// The Firestore query is Limit(limit+1): the requested page plus the one
	// probe row that reports whether another page exists (clause c).
	for _, v := range rows[start : start+boundedWindow(len(rows)-start, limit)] {
		u.countFetchedReads(kindRequirements, v.ID.String())
	}
	end := start + limit
	more := end < len(rows)
	if end > len(rows) {
		end = len(rows)
	}
	return rows[start:end], more, nil
}
func (u *unit) RequirementTexts(_ context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	// The Firestore counterpart reads one requirement document per id through
	// read(), which does not short-circuit on a staged write and caches a
	// NotFound as nil, so every requested id is charged, hit or miss.
	u.countFetchedReads(kindRequirements, ids...)
	for _, id := range ids {
		if v, ok := u.s.texts[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}
func (u *unit) IncrementsForRequirements(_ context.Context, ids []string) ([]domain.Increment, error) {
	// The Firestore counterpart is a batch get of one increment document per
	// requested id, which returns a snapshot for a missing document too, so
	// every requested id is charged and nothing else is.
	u.countFetchedReads(kindIncrements, ids...)
	filter := map[string]bool{}
	for _, id := range ids {
		filter[id] = true
	}
	all := len(ids) == 0
	out := make([]domain.Increment, 0, len(u.s.increments))
	for _, v := range u.s.increments {
		if all || filter[v.ID.String()] || filter[v.RequirementID.String()] {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}
func (u *unit) ExecutionsForIncrements(_ context.Context, ids []string) ([]domain.Execution, error) {
	filter := map[string]bool{}
	for _, id := range ids {
		filter[id] = true
	}
	out := make([]domain.Execution, 0, len(u.s.executions))
	for _, v := range u.s.executions {
		if filter[v.IncrementID.String()] {
			// The Firestore counterpart is a chunked IN query on the increment
			// index, so it is charged for the rows the query matched and not
			// for the rows this adapter walked past (dp-v2-087 d4).
			u.countFetchedReads(kindExecutions, v.ID.String())
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}
func (u *unit) EventsPage(_ context.Context, afterID string, limit int) ([]application.Event, bool, error) {
	rows := append([]application.Event(nil), u.s.events...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	start := 0
	for start < len(rows) && rows[start].ID <= afterID {
		start++
	}
	// Limit(limit+1), the same page-plus-probe shape as RequirementsPage.
	for _, v := range rows[start : start+boundedWindow(len(rows)-start, limit)] {
		u.countFetchedReads(kindEvents, v.ID)
	}
	end := start + limit
	more := end < len(rows)
	if end > len(rows) {
		end = len(rows)
	}
	return rows[start:end], more, nil
}
func (u *unit) QueueSummary(_ context.Context) (application.QueueSummary, error) {
	// The Firestore counterpart batch-gets all queueShards counter documents
	// and is charged for every one of them, present or absent. This adapter
	// derives the same answer from its maps, but it is settling the same
	// reservation, so it pays for the same documents.
	for shard := 0; shard < queueShards; shard++ {
		u.countFetchedReads(kindQueueCounters, shardKey(shard))
	}
	out := application.QueueSummary{ByRequirementStatus: map[string]int{}, ByIncrementStatus: map[string]int{}, Requirements: len(u.s.requirements), Increments: len(u.s.increments)}
	for _, v := range u.s.requirements {
		out.ByRequirementStatus[string(v.Status)]++
	}
	for _, v := range u.s.increments {
		out.ByIncrementStatus[string(v.Status)]++
	}
	for _, v := range u.s.executions {
		if v.Status == domain.ExecutionRunning || v.Status == domain.ExecutionStarting {
			out.ActiveExecutions++
		}
	}
	return out, nil
}
func (u *unit) SaveRequirement(_ context.Context, v domain.Requirement, expected domain.Version) error {
	key := v.ID.String()
	// The Firestore counterpart reads the document first (u.value), then
	// adjusts the sharded queue counter, then stages the aggregate.
	u.countKeyedRead(kindRequirements, key)
	old, ok := u.s.requirements[key]
	if (!ok && expected != 0) || (ok && old.Version != expected) {
		return ErrOptimisticConflict
	}
	u.countQueueCounter(key)
	v.Increments = append([]domain.IncrementID(nil), v.Increments...)
	u.countWrite(kindRequirements, key)
	u.s.requirements[key] = v
	return nil
}
func (u *unit) SaveRequirementText(_ context.Context, id, text string) error {
	// The text lives on the requirement document in Firestore, which is read
	// and re-staged: the same single record this adapter keys by id here.
	u.countKeyedRead(kindRequirements, id)
	u.countWrite(kindRequirements, id)
	u.s.texts[id] = text
	return nil
}
func (u *unit) RequirementText(_ context.Context, id string) (string, bool, error) {
	u.countKeyedRead(kindRequirements, id)
	v, ok := u.s.texts[id]
	return v, ok, nil
}
func (u *unit) Increment(_ context.Context, id string) (domain.Increment, bool, error) {
	u.countKeyedRead(kindIncrements, id)
	v, ok := u.s.increments[id]
	return v, ok, nil
}
func (u *unit) SaveIncrement(_ context.Context, v domain.Increment, expected domain.Version) error {
	key := v.ID.String()
	u.countKeyedRead(kindIncrements, key)
	old, ok := u.s.increments[key]
	if (!ok && expected != 0) || (ok && old.Version != expected) {
		return ErrOptimisticConflict
	}
	u.countQueueCounter(key)
	u.countWrite(kindIncrements, key)
	u.s.increments[key] = v
	return nil
}
func (u *unit) Execution(_ context.Context, id string) (domain.Execution, bool, error) {
	u.countKeyedRead(kindExecutions, id)
	v, ok := u.s.executions[id]
	return v, ok, nil
}
func (u *unit) SaveExecution(_ context.Context, v domain.Execution, expected domain.Version) error {
	key := v.ID.String()
	u.countKeyedRead(kindExecutions, key)
	old, ok := u.s.executions[key]
	if (!ok && expected != 0) || (ok && old.Version != expected) {
		return ErrOptimisticConflict
	}
	u.countQueueCounter(key)
	u.countWrite(kindExecutions, key)
	u.s.executions[key] = v
	return nil
}
func (u *unit) Lease(_ context.Context, id string) (domain.Lease, bool, error) {
	u.countKeyedRead(kindLeases, id)
	v, ok := u.s.leases[id]
	return v, ok, nil
}
func (u *unit) SaveLease(_ context.Context, v domain.Lease, expected domain.Version) error {
	key := v.ID.String()
	u.countKeyedRead(kindLeases, key)
	old, ok := u.s.leases[key]
	if (!ok && expected != 0) || (ok && old.Version != expected) {
		return ErrOptimisticConflict
	}
	u.countWrite(kindLeases, key)
	u.s.leases[key] = v
	return nil
}
func (u *unit) ActiveLeaseForIncrement(_ context.Context, id string) (domain.Lease, bool, error) {
	// The Firestore counterpart is queryWhere: an unfiltered scan of the
	// collection with the predicate applied to the decoded values, so it is
	// charged for every lease document and so is this one.
	u.countLeaseScan()
	for _, v := range u.s.leases {
		if v.IncrementID.String() == id && v.Status == domain.LeaseActive {
			return v, true, nil
		}
	}
	return domain.Lease{}, false, nil
}
func (u *unit) ActiveLeaseForIncrementAt(_ context.Context, id string, at time.Time) (domain.Lease, bool, error) {
	u.countLeaseScan()
	for _, v := range u.s.leases {
		if v.IncrementID.String() == id && v.Status == domain.LeaseActive && v.ActiveAt(at) {
			return v, true, nil
		}
	}
	return domain.Lease{}, false, nil
}
func (u *unit) ExpiredActiveLeases(_ context.Context, at time.Time, cursor string, limit int) ([]domain.Lease, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows := make([]domain.Lease, 0)
	for _, lease := range u.s.leases {
		if lease.Status == domain.LeaseActive && !lease.ExpiresAt.After(at) && lease.ID.String() > cursor {
			rows = append(rows, lease)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID.String() < rows[j].ID.String() })
	// Firestore applies both predicates and Limit(limit+1) in the query, so it
	// is charged for the page plus the probe row and no more.
	for _, lease := range rows[:boundedWindow(len(rows), limit)] {
		u.countFetchedReads(kindLeases, lease.ID.String())
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	next := ""
	if len(rows) == limit {
		next = rows[len(rows)-1].ID.String()
	}
	return rows, next, nil
}
func (u *unit) ActiveLeases(_ context.Context, limit int) ([]domain.Lease, error) {
	if limit <= 0 || limit > 101 {
		limit = 101
	}
	out := make([]domain.Lease, 0, limit)
	for _, lease := range u.s.leases {
		if lease.Status == domain.LeaseActive {
			// Where(active).Limit(limit+1) on the Firestore side: the matched
			// rows up to the probe row, never the whole collection.
			u.countFetchedReads(kindLeases, lease.ID.String())
			out = append(out, lease)
			if len(out) > limit {
				break
			}
		}
	}
	return out, nil
}
func (u *unit) ExecutionByLease(_ context.Context, leaseID string) (domain.Execution, bool, error) {
	// Firestore reads Where(lease index).Limit(2) -- the answer plus one row
	// that would prove the index ambiguous -- so at most two documents are
	// charged however many this adapter walks past.
	const bound = 2
	matched := make([]domain.Execution, 0, bound)
	for _, execution := range u.s.executions {
		if execution.LeaseID.String() == leaseID {
			matched = append(matched, execution)
			if len(matched) == bound {
				break
			}
		}
	}
	for _, execution := range matched {
		u.countFetchedReads(kindExecutions, execution.ID.String())
	}
	if len(matched) == 0 {
		return domain.Execution{}, false, nil
	}
	return matched[0], true, nil
}
func (u *unit) PendingControlProgresses(_ context.Context, limit int) ([]domain.ControlProgress, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	out := make([]domain.ControlProgress, 0, limit)
	for _, p := range u.s.controlProgress {
		if p.Verification == domain.VerificationPending {
			// Where(pending).Limit(limit) on the Firestore side. This is the
			// one bounded query with no probe row, so no +1 is charged.
			u.countFetchedReads(kindControlProgress, revisionKey(p.Revision))
			out = append(out, p)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}
func (u *unit) OutboxResolution(_ context.Context, leaseID string) (application.OutboxResolution, error) {
	var result application.OutboxResolution
	// Where(lease index).Limit(101) on the Firestore side: the matched rows up
	// to the safety bound, not the whole collection.
	const resolutionBound = 101
	matched := 0
	for _, item := range u.s.outbox {
		if item.LeaseID != leaseID {
			continue
		}
		if matched < resolutionBound {
			u.countFetchedReads(kindOutbox, item.ID)
			matched++
		}
		switch item.Status {
		case application.OutboxDelivered, application.OutboxConfirmed, application.OutboxSuperseded:
		case application.OutboxAmbiguous, application.OutboxReconciling, application.OutboxNeedsInput, application.OutboxDead:
			result.Ambiguous = true
		default:
			result.Pending = true
		}
	}
	return result, nil
}
func (u *unit) LatestLeaseForIncrement(_ context.Context, id string) (domain.Lease, bool, error) {
	u.countLeaseScan()
	var latest domain.Lease
	found := false
	for _, v := range u.s.leases {
		if v.IncrementID.String() == id && (!found || v.FencingToken > latest.FencingToken) {
			latest, found = v, true
		}
	}
	return latest, found, nil
}
func (u *unit) MaxFencingToken(_ context.Context, id string) (domain.FencingToken, error) {
	// Firestore answers this through LatestLeaseForIncrement, so it pays the
	// same unfiltered lease scan.
	u.countLeaseScan()
	var n domain.FencingToken
	for _, v := range u.s.leases {
		if v.IncrementID.String() == id && v.FencingToken > n {
			n = v.FencingToken
		}
	}
	return n, nil
}
func (u *unit) CanonicalTarget(_ context.Context, incrementID, _ string) (domain.ControlTarget, bool, error) {
	u.countKeyedRead(kindTargets, incrementID)
	v, ok := u.s.targets[incrementID]
	return v, ok, nil
}
func (u *unit) SaveCanonicalTarget(_ context.Context, incrementID string, target domain.ControlTarget) error {
	// Firestore stages this document without reading it first, so only the
	// write is charged.
	u.countWrite(kindTargets, incrementID)
	u.s.targets[incrementID] = target
	return nil
}
func (u *unit) Controls(_ context.Context) ([]domain.ControlIntent, error) {
	u.countControlScan()
	return append([]domain.ControlIntent(nil), u.s.controls...), nil
}
func (u *unit) SaveControl(_ context.Context, v domain.ControlIntent, expected domain.Revision) error {
	// Firestore resolves the current revision through ControlRevision -- an
	// unfiltered scan of the collection -- and then stages the new revision as
	// its own document.
	u.countControlScan()
	var cur domain.Revision
	for _, x := range u.s.controls {
		if x.Revision > cur {
			cur = x.Revision
		}
	}
	if cur != expected {
		return ErrOptimisticConflict
	}
	u.countWrite(kindControls, revisionKey(v.Revision))
	u.s.controls = append(u.s.controls, v)
	return nil
}
func (u *unit) ControlRevision(_ context.Context) (domain.Revision, error) {
	u.countControlScan()
	var n domain.Revision
	for _, v := range u.s.controls {
		if v.Revision > n {
			n = v.Revision
		}
	}
	return n, nil
}
func (u *unit) ControlProgress(_ context.Context, revision domain.Revision) (domain.ControlProgress, bool, error) {
	u.countKeyedRead(kindControlProgress, revisionKey(revision))
	v, ok := u.s.controlProgress[revision]
	return v, ok, nil
}
func (u *unit) SaveControlProgress(_ context.Context, value domain.ControlProgress, expected domain.ControlState) error {
	u.countKeyedRead(kindControlProgress, revisionKey(value.Revision))
	old, ok := u.s.controlProgress[value.Revision]
	if !ok {
		if expected != "" {
			return domain.ErrStaleVersion
		}
	} else if old.State != expected {
		return domain.ErrStaleVersion
	}
	u.countWrite(kindControlProgress, revisionKey(value.Revision))
	u.s.controlProgress[value.Revision] = value
	return nil
}
func (u *unit) SaveControlRequestedBy(_ context.Context, revision domain.Revision, value domain.RequestedBy) error {
	u.countKeyedRead(kindControlRequestedBy, revisionKey(revision))
	u.countWrite(kindControlRequestedBy, revisionKey(revision))
	u.s.controlRequestedBy[revision] = value
	return nil
}
func (u *unit) ControlRequestedBy(_ context.Context, revision domain.Revision) (domain.RequestedBy, bool, error) {
	u.countKeyedRead(kindControlRequestedBy, revisionKey(revision))
	v, ok := u.s.controlRequestedBy[revision]
	return v, ok, nil
}

// The installation concurrency limit side table (V2-068). It is keyed by
// Control Intent revision and written at most once per revision: a second write
// naming a different limit is a conflict, and an identical re-write is an
// idempotent replay, exactly as SaveRequirementRepositoryLink treats its
// write-once record.
func (u *unit) SaveAllocationLimit(_ context.Context, value application.AllocationLimit) error {
	if value.Revision == 0 {
		return errors.New("allocation limit requires a control revision")
	}
	if err := value.Validate(); err != nil {
		return err
	}
	u.countKeyedRead(kindAllocationLimits, revisionKey(value.Revision))
	if old, ok := u.s.allocationLimits[value.Revision]; ok {
		if old.InstallationConcurrentExecutions != value.InstallationConcurrentExecutions {
			return ErrOptimisticConflict
		}
		// An identical re-write stages nothing on either adapter, so no write
		// is charged for a replay.
		return nil
	}
	u.countWrite(kindAllocationLimits, revisionKey(value.Revision))
	u.s.allocationLimits[value.Revision] = value
	return nil
}
func (u *unit) AllocationLimit(_ context.Context, revision domain.Revision) (application.AllocationLimit, bool, error) {
	u.countKeyedRead(kindAllocationLimits, revisionKey(revision))
	v, ok := u.s.allocationLimits[revision]
	return v, ok, nil
}

// EffectiveAllocationLimit is the greatest revision that declared a limit. A
// later revision that declared none is simply absent from this table, so it
// cannot clear the owner's allocation policy. The scan is deterministic and
// reads no clock.
func (u *unit) EffectiveAllocationLimit(_ context.Context) (application.AllocationLimit, bool, error) {
	var best application.AllocationLimit
	found := false
	for revision := range u.s.allocationLimits {
		// Firestore answers this from u.query, an unfiltered scan of the
		// collection, so every declared limit is charged.
		u.countFetchedReads(kindAllocationLimits, revisionKey(revision))
	}
	for revision, v := range u.s.allocationLimits {
		if !found || revision > best.Revision {
			best, found = v, true
		}
	}
	return best, found, nil
}

// The needs-input question (V2-065) is one row per Requirement, and the same
// two rules both adapters share: the question half of an existing row can
// never be changed by a later save, and an answer already recorded can never
// be cleared. Only the answer fields may be added by a second transaction.
func (u *unit) SaveHumanInputRequest(_ context.Context, value application.HumanInputRequest) error {
	if err := application.ValidateHumanInputRequest(value); err != nil {
		return err
	}
	u.countKeyedRead(kindRequirementNeedsInput, value.RequirementID)
	if old, ok := u.s.humanInput[value.RequirementID]; ok {
		if !old.SameQuestion(value) {
			return domain.ErrStaleVersion
		}
		if old.Answered() && !value.Answered() {
			return domain.ErrStaleVersion
		}
	}
	u.countWrite(kindRequirementNeedsInput, value.RequirementID)
	u.s.humanInput[value.RequirementID] = value.Clone()
	return nil
}
func (u *unit) HumanInputRequest(_ context.Context, requirementID string) (application.HumanInputRequest, bool, error) {
	u.countKeyedRead(kindRequirementNeedsInput, requirementID)
	v, ok := u.s.humanInput[requirementID]
	if !ok {
		return application.HumanInputRequest{}, false, nil
	}
	return v.Clone(), true, nil
}

// Repository and its Observation use the same optimistic-concurrency shape
// as SaveRequirement above: a create must declare expected version 0 and a
// save must declare the stored version exactly.
func (u *unit) Repository(_ context.Context, id string) (domain.Repository, bool, error) {
	u.countKeyedRead(kindRepositories, id)
	v, ok := u.s.repositories[id]
	return v, ok, nil
}
func (u *unit) Repositories(_ context.Context) ([]domain.Repository, error) {
	out := make([]domain.Repository, 0, len(u.s.repositories))
	for k := range u.s.repositories {
		// u.query on the Firestore side: an unfiltered collection scan.
		u.countFetchedReads(kindRepositories, k)
	}
	for _, v := range u.s.repositories {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}
func (u *unit) SaveRepository(_ context.Context, v domain.Repository, expected domain.Version) error {
	key := v.ID.String()
	u.countKeyedRead(kindRepositories, key)
	old, ok := u.s.repositories[key]
	if (!ok && expected != 0) || (ok && old.Version != expected) {
		return ErrOptimisticConflict
	}
	u.countWrite(kindRepositories, key)
	u.s.repositories[key] = v
	return nil
}
func (u *unit) RepositoryObservation(_ context.Context, repositoryID string) (domain.RepositoryObservation, bool, error) {
	u.countKeyedRead(kindRepositoryObservations, repositoryID)
	v, ok := u.s.repositoryObs[repositoryID]
	return v, ok, nil
}
func (u *unit) SaveRepositoryObservation(_ context.Context, value domain.RepositoryObservation) error {
	// Firestore stages this Observation without reading it first.
	u.countWrite(kindRepositoryObservations, value.RepositoryID.String())
	u.s.repositoryObs[value.RepositoryID.String()] = value
	return nil
}

// The Requirement-to-Repository link is write-once, keyed by the Requirement.
// A second link naming a different Repository is a conflict, reported with
// the same ErrOptimisticConflict every other save in this adapter uses; an
// identical re-write is an idempotent replay and leaves exactly one record.
func (u *unit) SaveRequirementRepositoryLink(_ context.Context, value domain.RequirementRepositoryLink) error {
	if err := domain.ValidateRequirementRepositoryLink(value); err != nil {
		return err
	}
	key := value.RequirementID.String()
	u.countKeyedRead(kindRequirementRepositoryLinks, key)
	if old, ok := u.s.requirementRepo[key]; ok {
		if old.RepositoryID != value.RepositoryID {
			return ErrOptimisticConflict
		}
		// An identical re-write stages nothing on either adapter.
		return nil
	}
	u.countWrite(kindRequirementRepositoryLinks, key)
	u.s.requirementRepo[key] = value
	return nil
}
func (u *unit) RequirementRepositoryLink(_ context.Context, requirementID string) (domain.RequirementRepositoryLink, bool, error) {
	u.countKeyedRead(kindRequirementRepositoryLinks, requirementID)
	v, ok := u.s.requirementRepo[requirementID]
	return v, ok, nil
}
func (u *unit) RequirementRepositoryLinks(_ context.Context, ids []string) (map[string]domain.RequirementRepositoryLink, error) {
	out := make(map[string]domain.RequirementRepositoryLink, len(ids))
	for _, id := range ids {
		// Firestore answers this one keyed read at a time through
		// RequirementRepositoryLink, skipping the empty id, so a page of rows
		// with no link still costs one read per row.
		if id != "" {
			u.countKeyedRead(kindRequirementRepositoryLinks, id)
		}
		if v, ok := u.s.requirementRepo[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}
func (u *unit) RequirementIDsForRepository(_ context.Context, repositoryID string, limit int) ([]string, bool, error) {
	if repositoryID == "" {
		return nil, false, errors.New("repository id is required")
	}
	if limit <= 0 {
		return nil, false, nil
	}
	all := make([]string, 0, len(u.s.requirementRepo))
	for id, link := range u.s.requirementRepo {
		if link.RepositoryID.String() == repositoryID {
			all = append(all, id)
		}
	}
	sort.Strings(all)
	// Where(repository index).Limit(limit+1) on the Firestore side: the page
	// plus the one probe row that reports truncation.
	u.countFetchedReads(kindRequirementRepositoryLinks, all[:boundedWindow(len(all), limit)]...)
	if len(all) > limit {
		return all[:limit], true, nil
	}
	return all, false, nil
}

// The publication Observation (V2-072) is write-once, keyed by the operation
// identifier. An identical re-write is an idempotent replay and leaves exactly
// one record; a write that would change any recorded field is a conflict,
// reported with the same ErrOptimisticConflict every other save in this
// adapter uses. There is no update path at all: an external effect that was
// observed once is not re-observed into a different answer.
func (u *unit) SavePublicationObservation(_ context.Context, value domain.PublicationObservation) error {
	if err := domain.ValidatePublicationObservation(value); err != nil {
		return err
	}
	key := value.OperationID.String()
	u.countKeyedRead(kindPublicationObservations, key)
	if old, ok := u.s.publications[key]; ok {
		if old != value {
			return ErrOptimisticConflict
		}
		// An identical re-write stages nothing on either adapter.
		return nil
	}
	u.countWrite(kindPublicationObservations, key)
	u.s.publications[key] = value
	return nil
}
func (u *unit) PublicationObservation(_ context.Context, operationID string) (domain.PublicationObservation, bool, error) {
	u.countKeyedRead(kindPublicationObservations, operationID)
	v, ok := u.s.publications[operationID]
	return v, ok, nil
}

// PublicationObservationsForIncrement applies its bound to the answer and
// reports truncation, matching RequirementIDsForRepository above. The
// Increment an Observation belongs to is read out of its ref rather than out
// of a stored field, so the ref stays the single source of that association.
func (u *unit) PublicationObservationsForIncrement(_ context.Context, incrementID string, limit int) ([]domain.PublicationObservation, bool, error) {
	if incrementID == "" {
		return nil, false, errors.New("increment id is required")
	}
	if limit <= 0 {
		return nil, false, nil
	}
	all := make([]domain.PublicationObservation, 0, len(u.s.publications))
	for _, observation := range u.s.publications {
		increment, err := observation.IncrementID()
		if err != nil {
			return nil, false, err
		}
		if increment.String() == incrementID {
			all = append(all, observation)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].OperationID < all[j].OperationID })
	// Where(increment index).Limit(limit+1) on the Firestore side.
	for _, observation := range all[:boundedWindow(len(all), limit)] {
		u.countFetchedReads(kindPublicationObservations, observation.OperationID.String())
	}
	if len(all) > limit {
		return all[:limit], true, nil
	}
	return all, false, nil
}

func (u *unit) RunnerObservation(_ context.Context, runnerID string) (domain.RunnerObservation, bool, error) {
	u.countKeyedRead(kindRunnerObservations, runnerID)
	v, ok := u.s.runnerObservations[runnerID]
	v.Processes = append([]domain.ProcessObservation(nil), v.Processes...)
	return v, ok, nil
}
func (u *unit) SaveRunnerObservation(_ context.Context, value domain.RunnerObservation) error {
	// Firestore stages this Observation without reading it first.
	u.countWrite(kindRunnerObservations, value.RunnerID.String())
	value.Processes = append([]domain.ProcessObservation(nil), value.Processes...)
	u.s.runnerObservations[value.RunnerID.String()] = value
	return nil
}

// The Runner version report (V2-069) is its own RunnerID-keyed record,
// last-writer-wins: a Runner that switches binaries reports again and the new
// coordinates replace the old ones. There is no optimistic-concurrency
// version because the record has no state transition to protect.
func (u *unit) SaveRunnerVersionReport(_ context.Context, value application.RunnerVersionReport) error {
	if value.RunnerID == "" {
		return errors.New("runner id is required")
	}
	// Firestore stages this report without reading it first.
	u.countWrite(kindRunnerVersionReports, value.RunnerID)
	u.s.runnerVersions[value.RunnerID] = value
	return nil
}
func (u *unit) RunnerVersionReport(_ context.Context, runnerID string) (application.RunnerVersionReport, bool, error) {
	u.countKeyedRead(kindRunnerVersionReports, runnerID)
	v, ok := u.s.runnerVersions[runnerID]
	return v, ok, nil
}

// RunnerVersionReports enumerates every Runner this Control Plane has heard
// from -- the union of the Runners that have an Observation and the Runners
// that have a report -- and joins each with its report when one exists. A
// Runner with no report yields a row carrying only its id: no interval, no
// version and no digest is synthesized for it.
func (u *unit) RunnerVersionReports(_ context.Context, limit int) ([]application.RunnerVersionReport, bool, error) {
	if limit <= 0 {
		return nil, false, nil
	}
	ids := map[string]bool{}
	// Firestore answers this from TWO unfiltered collection scans -- the
	// reports and the Observations -- and is charged for every document in
	// both, which is why this join is bounded by the caller's limit but its
	// cost is not.
	for id := range u.s.runnerObservations {
		u.countFetchedReads(kindRunnerObservations, id)
		ids[id] = true
	}
	for id := range u.s.runnerVersions {
		u.countFetchedReads(kindRunnerVersionReports, id)
		ids[id] = true
	}
	out := make([]application.RunnerVersionReport, 0, len(ids))
	for id := range ids {
		if report, ok := u.s.runnerVersions[id]; ok {
			out = append(out, report)
			continue
		}
		out = append(out, application.RunnerVersionReport{RunnerID: id})
	}
	application.SortRunnerVersionReports(out)
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// ===========================================================================
// The Provider registry (V2-067)
// ===========================================================================
//
// One document per declared Provider, keyed by the Provider name, holding the
// bounded observation ring and the sticky verified instant; plus the
// assignment side table keyed by Execution id, with a per-Provider insertion
// ordered index the bounded enumeration reads. Neither read touches a
// collection whose size grows with the Requirement count.

func (u *unit) SaveProviderObservation(_ context.Context, value application.ProviderObservation) error {
	if !value.Provider.Valid() {
		return application.ErrProviderUnknown
	}
	if value.ObservedAt.IsZero() {
		return errors.New("provider observation requires an observed instant")
	}
	// The retention bound and the sticky verified instant are the shared write
	// rule, so this adapter cannot implement a retention the Firestore adapter
	// does not.
	//
	// Firestore keeps the ring and the assignment index in ONE Provider
	// registry document per Provider name, which it reads and re-stages, so
	// that single record is what is charged here too.
	u.countKeyedRead(kindProviderRegistry, string(value.Provider))
	u.countWrite(kindProviderRegistry, string(value.Provider))
	u.s.providerLogs[value.Provider] = application.ApplyProviderObservation(u.s.providerLogs[value.Provider], value)
	return nil
}

func (u *unit) ProviderObservations(_ context.Context, name application.ProviderName) (application.ProviderObservationLog, error) {
	if !name.Valid() {
		return application.ProviderObservationLog{}, application.ErrProviderUnknown
	}
	u.countKeyedRead(kindProviderRegistry, string(name))
	log, ok := u.s.providerLogs[name]
	if !ok {
		// Nothing is synthesized for a Provider with no record: an empty ring
		// and a zero VerifiedAt is what "never observed" looks like.
		return application.ProviderObservationLog{Provider: name}, nil
	}
	out := application.ProviderObservationLog{Provider: name, VerifiedAt: log.VerifiedAt}
	out.Observations = append([]application.ProviderObservation(nil), log.Observations...)
	return out, nil
}

func (u *unit) SaveProviderAssignment(_ context.Context, value application.ProviderAssignment) error {
	if !value.Provider.Valid() {
		return application.ErrProviderUnknown
	}
	if value.ExecutionID == "" {
		return errors.New("provider assignment requires an execution id")
	}
	u.countKeyedRead(kindProviderAssignments, value.ExecutionID)
	prior, existed := u.s.providerAssigns[value.ExecutionID]
	// The keyed side table is the record of which Provider an Execution was
	// started against; the per-Provider index below is what makes the bounded
	// enumeration a single keyed read.
	u.countWrite(kindProviderAssignments, value.ExecutionID)
	u.s.providerAssigns[value.ExecutionID] = value
	if existed && prior.Provider != value.Provider {
		// Firestore rewrites the losing Provider's registry document, which it
		// reads first.
		u.countKeyedRead(kindProviderRegistry, string(prior.Provider))
		u.countWrite(kindProviderRegistry, string(prior.Provider))
		kept := make([]application.ProviderAssignment, 0, len(u.s.providerAssignSeq[prior.Provider]))
		for _, a := range u.s.providerAssignSeq[prior.Provider] {
			if a.ExecutionID != value.ExecutionID {
				kept = append(kept, a)
			}
		}
		u.s.providerAssignSeq[prior.Provider] = kept
	}
	u.countKeyedRead(kindProviderRegistry, string(value.Provider))
	u.countWrite(kindProviderRegistry, string(value.Provider))
	u.s.providerAssignSeq[value.Provider] = application.AppendProviderAssignment(u.s.providerAssignSeq[value.Provider], value)
	return nil
}

func (u *unit) ProviderAssignment(_ context.Context, executionID string) (application.ProviderAssignment, bool, error) {
	u.countKeyedRead(kindProviderAssignments, executionID)
	v, ok := u.s.providerAssigns[executionID]
	return v, ok, nil
}

func (u *unit) ProviderAssignments(_ context.Context, name application.ProviderName) ([]application.ProviderAssignment, error) {
	if !name.Valid() {
		return nil, application.ErrProviderUnknown
	}
	// One keyed read of the Provider registry document, which is where
	// Firestore keeps this index.
	u.countKeyedRead(kindProviderRegistry, string(name))
	out := append([]application.ProviderAssignment(nil), u.s.providerAssignSeq[name]...)
	application.SortProviderAssignments(out)
	return out, nil
}

func (u *unit) Idempotency(_ context.Context, id, op string) (application.IdempotentResponse, bool, error) {
	u.countKeyedRead(kindIdempotency, id)
	v, ok := u.s.requests[id]
	if ok && v.Operation != op {
		return v, true, application.ErrIdempotencyConflict
	}
	return v, ok, nil
}
func (u *unit) SaveIdempotency(_ context.Context, v application.IdempotentResponse) error {
	u.countKeyedRead(kindIdempotency, v.RequestID)
	if old, ok := u.s.requests[v.RequestID]; ok && (old.Operation != v.Operation || old.Fingerprint != v.Fingerprint) {
		return application.ErrIdempotencyConflict
	}
	u.countWrite(kindIdempotency, v.RequestID)
	u.s.requests[v.RequestID] = v
	return nil
}
func (u *unit) Record(e application.Event, o *application.OutboxItem) error {
	// Firestore stages the Event document, and the Outbox document when there
	// is one, without reading either.
	u.countWrite(kindEvents, e.ID)
	u.s.events = append(u.s.events, e)
	if o != nil {
		u.countWrite(kindOutbox, o.ID)
		v := *o
		if !v.Status.Valid() {
			return application.ErrInvalidOutbox
		}
		if v.Version == 0 {
			v.Version = 1
		}
		if v.Status == "" {
			v.Status = application.OutboxPending
		}
		v.Payload = append([]byte(nil), o.Payload...)
		u.s.outbox = append(u.s.outbox, v)
	}
	return nil
}

func (u *unit) Outbox(_ context.Context, id string) (application.OutboxItem, bool, error) {
	// One keyed read on the Firestore side, whatever this adapter's slice scan
	// costs.
	u.countKeyedRead(kindOutbox, id)
	for _, v := range u.s.outbox {
		if v.ID == id {
			if !v.Status.Valid() {
				return application.OutboxItem{}, false, application.ErrInvalidOutbox
			}
			v.Payload = append([]byte(nil), v.Payload...)
			return v, true, nil
		}
	}
	return application.OutboxItem{}, false, nil
}

func (u *unit) Outboxes(_ context.Context, now time.Time, limit int) ([]application.OutboxItem, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows := make([]application.OutboxItem, 0, len(u.s.outbox))
	// The Firestore query is Where(outbox_status in {pending, waiting,
	// delivering}).Limit(MaxQueryRows+1), and the due-time test is applied to
	// the decoded rows afterwards, so what is charged is the status-matched
	// window and not this adapter's wider ready set.
	const scanBound = 1001
	scanned := 0
	for _, v := range u.s.outbox {
		if !v.Status.Valid() {
			return nil, application.ErrInvalidOutbox
		}
		status := v.Status
		if status == "" {
			status = application.OutboxPending
		}
		if scanned < scanBound {
			switch status {
			case application.OutboxPending, application.OutboxWaiting, application.OutboxDelivering:
				u.countFetchedReads(kindOutbox, v.ID)
				scanned++
			}
		}
		ready := status == application.OutboxPending || status == application.OutboxAmbiguous || status == application.OutboxReconciling || (status == application.OutboxWaiting && (v.NextAttemptAt.IsZero() || !v.NextAttemptAt.After(now))) || (status == application.OutboxDelivering && !v.DeliveryLeaseUntil.IsZero() && !v.DeliveryLeaseUntil.After(now))
		if ready {
			v.Status = status
			v.Payload = append([]byte(nil), v.Payload...)
			rows = append(rows, v)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (u *unit) SaveOutbox(_ context.Context, value application.OutboxItem, expected domain.Version) error {
	if value.ID == "" || !value.Status.Valid() {
		return application.ErrInvalidOutbox
	}
	if value.Status == "" {
		value.Status = application.OutboxPending
	}
	// One keyed read and one staged write on the Firestore side, whatever this
	// adapter's slice scan costs.
	u.countKeyedRead(kindOutbox, value.ID)
	u.countWrite(kindOutbox, value.ID)
	for i, old := range u.s.outbox {
		if old.ID != value.ID {
			continue
		}
		if old.Version == 0 {
			old.Version = 1
		}
		if expected != old.Version {
			return domain.ErrStaleVersion
		}
		value.Version = old.Version + 1
		value.Payload = append([]byte(nil), value.Payload...)
		u.s.outbox[i] = value
		return nil
	}
	if expected != 0 {
		return domain.ErrStaleVersion
	}
	if value.Version == 0 {
		value.Version = 1
	}
	u.s.outbox = append(u.s.outbox, value)
	return nil
}

func (m *Store) Requirement(id string) (domain.Requirement, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data.requirements[id]
	v.Increments = append([]domain.IncrementID(nil), v.Increments...)
	return v, ok
}
func (m *Store) Increment(id string) (domain.Increment, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data.increments[id]
	return v, ok
}
func (m *Store) RequirementText(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data.texts[id]
	return v, ok
}
func (m *Store) Execution(id string) (domain.Execution, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data.executions[id]
	return v, ok
}
func (m *Store) Lease(id string) (domain.Lease, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data.leases[id]
	return v, ok
}
func (m *Store) Events() []application.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]application.Event(nil), m.data.events...)
}
func (m *Store) Outbox() []application.OutboxItem {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]application.OutboxItem, 0, len(m.data.outbox))
	for _, v := range m.data.outbox {
		v.Payload = append([]byte(nil), v.Payload...)
		out = append(out, v)
	}
	return out
}

func (m *Store) OutboxByID(id string) (application.OutboxItem, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.data.outbox {
		if v.ID == id {
			v.Payload = append([]byte(nil), v.Payload...)
			return v, true
		}
	}
	return application.OutboxItem{}, false
}
func (m *Store) Controls() []domain.ControlIntent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domain.ControlIntent(nil), m.data.controls...)
}
func (m *Store) Idempotency(requestID string) (application.IdempotentResponse, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data.requests[requestID]
	v.ResponseJSON = append([]byte(nil), v.ResponseJSON...)
	return v, ok
}

// ===========================================================================
// Quota inspection seams (V2-087)
// ===========================================================================
//
// These two exported inspectors mirror the helpers internal/store/firestore's
// own tests already keep for the same reason (readQuotaRecord and
// seedQuotaTotal in firestore/quota_integration_test.go): once a reservation is
// settled, a test can no longer fill a day's budget by reserving inside an
// otherwise empty transaction, because the true-up credits that fill straight
// back down to its own tiny actual cost. Seeding the committed total directly,
// outside ReserveQuota, is what lets a test set up a precise pre-existing day.
//
// They are exported, unlike the Firestore package's file-local pair, because
// the test that needs them lives in another package: internal/api's
// quota-exhaustion route assertion.

// QuotaTotal reports the committed daily usage total for at's UTC day, and
// whether this store holds a record for that day at all. A day that has never
// been reserved against reports false rather than a zero total, so "nothing
// was reserved today" stays distinguishable from "the total is zero".
func (m *Store) QuotaTotal(at time.Time) (quota.Usage, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.quota.Day != quota.Day(at) {
		return quota.Usage{}, false
	}
	return m.data.quota.Total, true
}

// SeedQuotaTotal sets the committed daily total for at's UTC day directly,
// bypassing ReserveQuota and therefore bypassing the true-up. It is the
// counterpart of the Firestore package's seedQuotaTotal and exists for the
// same reason: a synthetic fill made through ReserveQuota would be credited
// back to its own actual cost, so it could no longer set up an exhausted day.
// The accounting buckets are left empty on purpose: the daily aggregate is the
// sole hard-budget source of truth, and inventing a bucket distribution for a
// synthetic total would fabricate an audit dimension nothing measured.
func (m *Store) SeedQuotaTotal(at time.Time, total quota.Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data.quota = quota.Counter{Day: quota.Day(at), Total: total}
}
