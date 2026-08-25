// Package firestore implements application UnitOfWork on a real Cloud
// Firestore transaction. Writes are staged until the callback returns: this
// preserves Firestore's read-before-write rule and gives read-your-writes.
package firestore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	cloudfirestore "cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/quota"
)

const MaxWrites = 400
const MaxQueryRows = 1000
const RecordSchema = "v1"
const MaxPathKeyBytes = 512
const queueShards = 32

var (
	ErrInvalidSchema = errors.New("invalid record schema")
	ErrWriteCap      = errors.New("firestore write cap exceeded")
	ErrQueryLimit    = errors.New("firestore query result exceeds bounded limit")
)

// ---------------------------------------------------------------------------
// The record envelope: one accepted set, one predicate, one payload gate.
//
// RecordSchema above is the single value this binary WRITES. The set below is
// what this binary will READ. The two are deliberately separate declarations:
// widening what a reader accepts is the expand stage of
// docs/operations/self-update.md section 7.1, and bumping what a writer emits
// is a separate Increment. See docs/operations/record-envelope.md.
// ---------------------------------------------------------------------------

// AcceptedRecordSchemas is the ordered, closed, duplicate-free set of record
// schema (envelope) ids this binary will read. It ships with exactly one
// member -- the value the encode paths already write -- because accepting an
// id no writer in this repository can produce would admit a document whose
// payload the running code cannot interpret. The member that admits a second
// envelope id belongs to the Increment that introduces the second writer.
//
// It is a package-level variable rather than a constant expression solely so
// a test can substitute a wider set and restore it with defer, the technique
// internal/scheduler/priority.go already uses for scoreFn. Nothing outside a
// test may reassign it.
var AcceptedRecordSchemas = []string{RecordSchema}

// RecordSchemaAccepted is the one read-side envelope decision in this
// package: all five read-side refusal sites call it and none compares an
// envelope value itself. Membership is exact string equality. No trimming,
// case folding, prefix matching or normalisation of any kind is applied,
// because every such normalisation widens the accepted set invisibly.
func RecordSchemaAccepted(id string) bool {
	for _, accepted := range AcceptedRecordSchemas {
		if id == accepted {
			return true
		}
	}
	return false
}

// recordSchemaIsNative reports whether id is the value this binary writes. It
// is the only read-side predicate that names RecordSchema, and it exists so
// the payload-interpretability gate below can leave today's read path
// untouched.
func recordSchemaIsNative(id string) bool { return id == RecordSchema }

// validateRecordPayload is a package-level variable rather than a direct call
// to domain.Validate solely so a test can count invocations and prove that
// the native read path invokes it zero times. Nothing outside a test may
// reassign it.
var validateRecordPayload = domain.Validate

// domainValidatable returns the value the domain's validity predicate should
// be applied to, and whether the repository declares one for out's type at
// all. domain.Validate describes Requirement, Increment, Execution and Lease
// and refuses every other type with "unsupported domain value", so it must
// only be handed values it describes; for every other record kind this
// package stores (outbox, event, queue counter, quota, idempotency, text) the
// repository declares no validity predicate and the gate is a no-op.
func domainValidatable(out any) (any, bool) {
	switch v := out.(type) {
	case *requirementRecord:
		if v == nil {
			return nil, false
		}
		return &v.Requirement, true
	case *domain.Requirement, *domain.Increment, *domain.Execution, *domain.Lease:
		return v, true
	}
	return nil, false
}

// requireInterpretablePayload refuses a decoded payload that carries a
// NON-NATIVE envelope id and does not satisfy the domain's validity predicate
// for its type. Unknown-field tolerance is what makes an expand stage
// reversible and is also what makes silent acceptance possible: a payload
// written under a later envelope that renamed a field decodes into a
// zero-valued struct and looks like a legitimate record rather than an error.
//
// The gate is deliberately NOT applied to the native id. Today's read path is
// therefore unchanged, which is the only form in which every pre-existing
// store test keeps passing under its own name with its own assertions.
func requireInterpretablePayload(envelope string, out any) error {
	if recordSchemaIsNative(envelope) {
		return nil
	}
	value, ok := domainValidatable(out)
	if !ok {
		return nil
	}
	if err := validateRecordPayload(value); err != nil {
		return ErrInvalidSchema
	}
	return nil
}

// decodePayload is the single read-side seam that turns a stored payload into
// a typed value: it decodes, then applies the interpretability gate.
func decodePayload(envelope string, payload []byte, out any) error {
	if err := json.Unmarshal(payload, out); err != nil {
		return ErrInvalidSchema
	}
	return requireInterpretablePayload(envelope, out)
}

// requireInterpretableScannedPayload is decodePayload for a bounded scan site
// that knows the record kind but not the caller's concrete type. A document
// that fails the gate fails the WHOLE scan, exactly as an envelope failure or
// a JSON failure does today; no partial-scan or skip-the-bad-document
// semantics is introduced.
func requireInterpretableScannedPayload(envelope, kind string, payload []byte) error {
	if recordSchemaIsNative(envelope) {
		return nil
	}
	out := scannedPayloadPrototype(kind)
	if out == nil {
		return nil
	}
	return decodePayload(envelope, payload, out)
}

// scannedPayloadPrototype returns a fresh typed value for the record kinds the
// domain declares a validity predicate for, and nil for every other kind.
func scannedPayloadPrototype(kind string) any {
	switch kind {
	case "requirement":
		return &requirementRecord{}
	case "increment":
		return &domain.Increment{}
	case "execution":
		return &domain.Execution{}
	case "lease":
		return &domain.Lease{}
	}
	return nil
}

// scannedRow is one document a bounded scan accepted: its envelope id and its
// payload, carried together so the typed decode below can apply the
// interpretability gate for the envelope the document actually declared.
type scannedRow struct {
	envelope string
	payload  []byte
}

func PathKey(value string) (string, error) {
	if strings.TrimSpace(value) == "" || !utf8.ValidString(value) {
		return "", errors.New("empty path key")
	}
	if len([]byte(value)) > MaxPathKeyBytes {
		return "", errors.New("path key exceeds byte limit")
	}
	return base64.RawURLEncoding.EncodeToString([]byte(value)), nil
}
func CollectionPath(installation, collection string) (string, error) {
	i, err := PathKey(installation)
	if err != nil {
		return "", err
	}
	if collection == "" {
		return "", errors.New("empty collection")
	}
	return "installations/" + i + "/" + collection, nil
}

// EncodeRecord/DecodeRecord are the stable JSON codec used by export and
// migration tools. Firestore stores the payload as a JSON string in the same
// envelope, preventing implicit Firestore value coercion.
func EncodeRecord(kind string, value any) ([]byte, error) {
	if kind == "" {
		return nil, ErrInvalidSchema
	}
	return json.Marshal(map[string]any{"record_schema": RecordSchema, "kind": kind, "value": value})
}
func DecodeRecord(data []byte, expectedKind string, out any) error {
	var e struct {
		Schema string          `json:"record_schema"`
		Kind   string          `json:"kind"`
		Value  json.RawMessage `json:"value"`
	}
	if expectedKind == "" {
		return ErrInvalidSchema
	}
	if err := json.Unmarshal(data, &e); err != nil || !RecordSchemaAccepted(e.Schema) || e.Kind != expectedKind || len(e.Value) == 0 {
		return ErrInvalidSchema
	}
	if err := decodePayload(e.Schema, e.Value, out); err != nil {
		return err
	}
	return nil
}

type document struct {
	RecordSchema        string `firestore:"record_schema"`
	Kind                string `firestore:"kind"`
	Payload             string `firestore:"payload"`
	OutboxStatus        string `firestore:"outbox_status,omitempty"`
	OutboxNextAttemptAt string `firestore:"outbox_next_attempt_at,omitempty"`
	OutboxLeaseUntil    string `firestore:"outbox_lease_until,omitempty"`
	IndexIncrementID    string `firestore:"index_increment_id,omitempty"`
	IndexLeaseID        string `firestore:"index_lease_id,omitempty"`
	IndexRepositoryID   string `firestore:"index_repository_id,omitempty"`
	LeaseStatus         string `firestore:"lease_status,omitempty"`
	LeaseExpiresAt      string `firestore:"lease_expires_at,omitempty"`
	ControlVerification string `firestore:"control_verification,omitempty"`
}

type requirementRecord struct {
	Requirement domain.Requirement `json:"requirement"`
	Text        string             `json:"text,omitempty"`
}
type queueCounter struct {
	Schema           string         `json:"schema"`
	Requirements     map[string]int `json:"requirements"`
	Increments       map[string]int `json:"increments"`
	ActiveExecutions int            `json:"active_executions"`
}

type quotaRecord struct {
	Day    string                    `json:"day"`
	Total  quota.Usage               `json:"total"`
	Shards [quota.Shards]quota.Usage `json:"shards"`
}

func encodeDocument(kind string, value any) (document, error) {
	if kind == "" {
		return document{}, ErrInvalidSchema
	}
	b, err := json.Marshal(value)
	if err != nil {
		return document{}, err
	}
	d := document{RecordSchema: RecordSchema, Kind: kind, Payload: string(b)}
	if v, ok := value.(domain.Execution); ok {
		d.IndexIncrementID = v.IncrementID.String()
		d.IndexLeaseID = v.LeaseID.String()
	}
	// The Requirement-to-Repository link is projected onto one indexed
	// field so the per-repository read is a single-field equality query.
	// Firestore indexes every single field automatically, so
	// firestore.indexes.json (which declares only the composite indexes for
	// outbox and leases) needs no entry for it.
	if v, ok := value.(domain.RequirementRepositoryLink); ok {
		d.IndexRepositoryID = v.RepositoryID.String()
	}
	if v, ok := value.(domain.Lease); ok {
		d.LeaseStatus = string(v.Status)
		if !v.ExpiresAt.IsZero() {
			d.LeaseExpiresAt = v.ExpiresAt.UTC().Format(time.RFC3339Nano)
		}
	}
	if kind == "outbox" {
		v, ok := value.(application.OutboxItem)
		if !ok {
			return document{}, ErrInvalidSchema
		}
		status := v.Status
		if status == "" {
			status = application.OutboxPending
		}
		if !status.Valid() {
			return document{}, application.ErrInvalidOutbox
		}
		d.OutboxStatus = string(status)
		d.IndexLeaseID = v.LeaseID
		if !v.NextAttemptAt.IsZero() {
			d.OutboxNextAttemptAt = v.NextAttemptAt.UTC().Format(time.RFC3339Nano)
		}
		if !v.DeliveryLeaseUntil.IsZero() {
			d.OutboxLeaseUntil = v.DeliveryLeaseUntil.UTC().Format(time.RFC3339Nano)
		}
	}
	if kind == "control-progress" {
		v, ok := value.(domain.ControlProgress)
		if !ok {
			return document{}, ErrInvalidSchema
		}
		d.ControlVerification = string(v.Verification)
	}
	return d, nil
}
func decodeDocument(snap *cloudfirestore.DocumentSnapshot, expectedKind string, out any) error {
	if snap == nil || !snap.Exists() {
		return nil
	}
	var d document
	if err := snap.DataTo(&d); err != nil || !RecordSchemaAccepted(d.RecordSchema) || d.Kind != expectedKind || d.Payload == "" {
		return ErrInvalidSchema
	}
	if err := decodePayload(d.RecordSchema, []byte(d.Payload), out); err != nil {
		return err
	}
	return nil
}

type pending struct {
	ref    *cloudfirestore.DocumentRef
	doc    document
	create bool
}
type unit struct {
	store  *Store
	tx     *cloudfirestore.Transaction
	ctx    context.Context
	cache  map[string]*cloudfirestore.DocumentSnapshot
	values map[string]pending
	// quotaRef/quotaKey/quotaReserved record the single worst-case
	// ReserveQuota call made at the start of this transaction (before any
	// other mutation was staged), so flush() can true it up to the actual
	// cost right before committing.
	quotaRef      *cloudfirestore.DocumentRef
	quotaKey      string
	quotaReserved quota.Usage
}

// Store is a tenant-scoped Firestore adapter. It has no canonical in-memory
// state; all application records belong to Firestore.
type Store struct {
	client       *cloudfirestore.Client
	installation string
}

func NewStore(client *cloudfirestore.Client, installation string) (*Store, error) {
	if client == nil || installation == "" {
		return nil, errors.New("firestore client and installation are required")
	}
	if os.Getenv("FIRESTORE_EMULATOR_HOST") != "" {
		return nil, errors.New("production Firestore constructor refuses emulator host; use NewEmulatorStore")
	}
	if _, err := PathKey(installation); err != nil {
		return nil, err
	}
	return &Store{client: client, installation: installation}, nil
}

// NewEmulatorStore is the only constructor permitted to use a Firestore
// emulator. Tests must opt in explicitly instead of accidentally redirecting
// a production adapter through FIRESTORE_EMULATOR_HOST.
func NewEmulatorStore(client *cloudfirestore.Client, installation string) (*Store, error) {
	if client == nil || installation == "" {
		return nil, errors.New("firestore client and installation are required")
	}
	if _, err := PathKey(installation); err != nil {
		return nil, err
	}
	return &Store{client: client, installation: installation}, nil
}
func NewWithClient(client *cloudfirestore.Client, installation string) (*Store, error) {
	return NewStore(client, installation)
}
func (s *Store) InstallationRoot() (string, error) {
	i, err := PathKey(s.installation)
	if err != nil {
		return "", err
	}
	return "installations/" + i, nil
}
func (s *Store) path(collection, id string) (*cloudfirestore.DocumentRef, error) {
	root, err := CollectionPath(s.installation, collection)
	if err != nil {
		return nil, err
	}
	key, err := PathKey(id)
	if err != nil {
		return nil, err
	}
	return s.client.Doc(root + "/" + key), nil
}

func (s *Store) Transact(ctx context.Context, fn func(application.UnitOfWork) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.client.RunTransaction(ctx, func(ctx context.Context, tx *cloudfirestore.Transaction) error {
		u := &unit{store: s, tx: tx, ctx: ctx, cache: map[string]*cloudfirestore.DocumentSnapshot{}, values: map[string]pending{}}
		if err := fn(u); err != nil {
			return err
		}
		return u.flush()
	})
}

// AuthorityContext lets application event recording reuse the timestamp
// captured before a transaction callback, including Firestore retry attempts.
func (u *unit) AuthorityContext() context.Context { return u.ctx }

// ReserveQuota keeps a daily aggregate and 32 accounting buckets in the same
// Firestore transaction as the caller's mutation. The aggregate is the sole
// hard-budget source of truth; buckets are audit dimensions in that document,
// not a claim of Firestore contention sharding.
func (u *unit) ReserveQuota(ctx context.Context, key string, at time.Time, usage quota.Usage) error {
	ref, err := u.store.path("quota", quota.Day(at))
	if err != nil {
		return err
	}
	var record quotaRecord
	if ok, err := u.value(ref, "quota", &record); err != nil {
		return err
	} else if !ok {
		record.Day = quota.Day(at)
	}
	counter := quota.Counter{Day: record.Day, Total: record.Total, Shards: record.Shards}
	if err := counter.Reserve(at, key, usage, quota.DefaultBudget); err != nil {
		return err
	}
	record = quotaRecord{Day: counter.Day, Total: counter.Total, Shards: counter.Shards}
	if err := u.stage(ref, "quota", record, false); err != nil {
		return err
	}
	// Remember the worst-case reservation so flush() can true it up to the
	// actual read/write/delete cost right before the transaction commits.
	// ReserveQuota is called exactly once per transaction, before any other
	// mutation is staged (internal/application.Service.transact/mutate).
	u.quotaRef = ref
	u.quotaKey = key
	u.quotaReserved = usage
	return nil
}

// trueUpQuota corrects the worst-case reservation staged by ReserveQuota
// down to the transaction's actual cost: reads actually observed in the
// unit-of-work cache (every document this transaction fetched, by query or
// by key, is cached exactly once by path) and writes actually staged (this
// adapter has no delete operation today, so actual deletes is always zero).
// It must run after the caller's callback returns and before flush() issues
// any Firestore write, so the corrected total is what is actually committed.
func (u *unit) trueUpQuota() error {
	if u.quotaRef == nil {
		return nil
	}
	p, ok := u.values[u.quotaRef.Path]
	if !ok {
		return nil
	}
	var record quotaRecord
	if err := json.Unmarshal([]byte(p.doc.Payload), &record); err != nil {
		return ErrInvalidSchema
	}
	actual := quota.Usage{Reads: int64(len(u.cache)), Writes: int64(len(u.values))}
	counter := quota.Counter{Day: record.Day, Total: record.Total, Shards: record.Shards}
	counter.TrueUp(u.quotaKey, u.quotaReserved, actual)
	record = quotaRecord{Day: counter.Day, Total: counter.Total, Shards: counter.Shards}
	return u.stage(u.quotaRef, "quota", record, false)
}
func (u *unit) read(ref *cloudfirestore.DocumentRef) (*cloudfirestore.DocumentSnapshot, error) {
	if v, ok := u.cache[ref.Path]; ok {
		return v, nil
	}
	v, err := u.tx.Get(ref)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			u.cache[ref.Path] = nil
			return nil, nil
		}
		return nil, err
	}
	u.cache[ref.Path] = v
	return v, nil
}

// countReads caches every snapshot a query or batch get fetched, exactly
// like read() does for a single document lookup. The end-of-transaction
// true-up measures actual reads by cache size, so every read path must feed
// this same cache or the true-up would under-count a query's real cost.
func (u *unit) countReads(snaps []*cloudfirestore.DocumentSnapshot) {
	for _, snap := range snaps {
		if snap == nil {
			continue
		}
		u.cache[snap.Ref.Path] = snap
	}
}
func (u *unit) value(ref *cloudfirestore.DocumentRef, kind string, out any) (bool, error) {
	if p, ok := u.values[ref.Path]; ok {
		if err := json.Unmarshal([]byte(p.doc.Payload), out); err != nil {
			return false, ErrInvalidSchema
		}
		return true, nil
	}
	s, err := u.read(ref)
	if err != nil {
		return false, err
	}
	if s == nil || !s.Exists() {
		return false, nil
	}
	if err := decodeDocument(s, kind, out); err != nil {
		return false, err
	}
	return true, nil
}
func (u *unit) stage(ref *cloudfirestore.DocumentRef, kind string, value any, create bool) error {
	d, err := encodeDocument(kind, value)
	if err != nil {
		return err
	}
	if _, ok := u.values[ref.Path]; !ok && len(u.values) >= MaxWrites {
		return ErrWriteCap
	}
	if prior, ok := u.values[ref.Path]; ok && prior.create {
		create = true
	}
	u.values[ref.Path] = pending{ref: ref, doc: d, create: create}
	return nil
}
func (u *unit) saveVersion(ref *cloudfirestore.DocumentRef, kind string, value any, expected, current domain.Version) error {
	if current != expected {
		return domain.ErrStaleVersion
	}
	return u.stage(ref, kind, value, expected == 0 && current == 0)
}
func (u *unit) flush() error {
	if err := u.trueUpQuota(); err != nil {
		return err
	}
	if len(u.values) > MaxWrites {
		return ErrWriteCap
	}
	paths := make([]string, 0, len(u.values))
	for p := range u.values {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		v := u.values[p]
		ref := v.ref
		var err error
		if v.create {
			err = u.tx.Create(ref, v.doc)
		} else {
			err = u.tx.Set(ref, v.doc)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (u *unit) Requirement(ctx context.Context, id string) (domain.Requirement, bool, error) {
	ref, err := u.store.path("requirements", id)
	if err != nil {
		return domain.Requirement{}, false, err
	}
	var v requirementRecord
	ok, err := u.value(ref, "requirement", &v)
	return v.Requirement, ok, err
}
func (u *unit) Requirements(ctx context.Context) ([]domain.Requirement, error) {
	rows, err := u.query(ctx, "requirements", "requirement")
	if err != nil {
		return nil, err
	}
	out := make([]domain.Requirement, 0, len(rows))
	for _, b := range rows {
		var v requirementRecord
		if json.Unmarshal(b, &v) != nil {
			return nil, ErrInvalidSchema
		}
		out = append(out, v.Requirement)
	}
	return out, nil
}

func (u *unit) RequirementsPage(ctx context.Context, afterID string, limit int) ([]domain.Requirement, bool, error) {
	if limit <= 0 || limit > application.MaxPageSize {
		return nil, false, fmt.Errorf("invalid requirement page limit")
	}
	path, err := CollectionPath(u.store.installation, "requirements")
	if err != nil {
		return nil, false, err
	}
	q := u.store.client.Collection(path).OrderBy(cloudfirestore.DocumentID, cloudfirestore.Asc)
	if afterID != "" {
		key, e := PathKey(afterID)
		if e != nil {
			return nil, false, e
		}
		q = q.StartAfter(path + "/" + key)
	}
	snaps, err := u.tx.Documents(q.Limit(limit + 1)).GetAll()
	if err != nil {
		return nil, false, err
	}
	u.countReads(snaps)
	more := len(snaps) > limit
	if more {
		snaps = snaps[:limit]
	}
	out := make([]domain.Requirement, 0, len(snaps))
	for _, snap := range snaps {
		var v requirementRecord
		if err := decodeDocument(snap, "requirement", &v); err != nil {
			return nil, false, err
		}
		out = append(out, v.Requirement)
	}
	return out, more, nil
}
func (u *unit) RequirementTexts(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	// One bounded transaction read for the page, rather than a read per row.
	refs := make([]*cloudfirestore.DocumentRef, 0, len(ids))
	for _, id := range ids {
		ref, err := u.store.path("requirements", id)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	for i, ref := range refs {
		snap, err := u.read(ref)
		if err != nil {
			return nil, err
		}
		if snap == nil || !snap.Exists() {
			continue
		}
		var v requirementRecord
		if err := decodeDocument(snap, "requirement", &v); err != nil {
			return nil, err
		}
		out[ids[i]] = v.Text
	}
	return out, nil
}
func (u *unit) IncrementsForRequirements(ctx context.Context, ids []string) ([]domain.Increment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	refs := make([]*cloudfirestore.DocumentRef, 0, len(ids))
	for _, id := range ids {
		r, e := u.store.path("increments", id)
		if e != nil {
			return nil, e
		}
		refs = append(refs, r)
	}
	snaps, err := u.tx.GetAll(refs)
	if err != nil {
		return nil, err
	}
	u.countReads(snaps)
	out := make([]domain.Increment, 0, len(snaps))
	for _, snap := range snaps {
		if snap == nil || !snap.Exists() {
			continue
		}
		var v domain.Increment
		if err := decodeDocument(snap, "increment", &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}
func (u *unit) ExecutionsForIncrements(ctx context.Context, ids []string) ([]domain.Execution, error) {
	path, err := CollectionPath(u.store.installation, "executions")
	if err != nil {
		return nil, err
	}
	out := make([]domain.Execution, 0)
	if len(ids) == 0 {
		return out, nil
	}
	for start := 0; start < len(ids); start += 30 {
		end := start + 30
		if end > len(ids) {
			end = len(ids)
		}
		vals := make([]any, end-start)
		for i := range ids[start:end] {
			vals[i] = ids[start+i]
		}
		snaps, e := u.tx.Documents(u.store.client.Collection(path).Where("index_increment_id", "in", vals).Limit(MaxQueryRows + 1)).GetAll()
		if e != nil {
			return nil, e
		}
		u.countReads(snaps)
		if len(snaps) > MaxQueryRows {
			return nil, ErrQueryLimit
		}
		for _, snap := range snaps {
			var v domain.Execution
			if e := decodeDocument(snap, "execution", &v); e != nil {
				return nil, e
			}
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}
func (u *unit) EventsPage(ctx context.Context, afterID string, limit int) ([]application.Event, bool, error) {
	path, err := CollectionPath(u.store.installation, "events")
	if err != nil {
		return nil, false, err
	}
	q := u.store.client.Collection(path).OrderBy(cloudfirestore.DocumentID, cloudfirestore.Asc)
	if afterID != "" {
		key, e := PathKey(afterID)
		if e != nil {
			return nil, false, e
		}
		q = q.StartAfter(path + "/" + key)
	}
	snaps, e := u.tx.Documents(q.Limit(limit + 1)).GetAll()
	if e != nil {
		return nil, false, e
	}
	u.countReads(snaps)
	more := len(snaps) > limit
	if more {
		snaps = snaps[:limit]
	}
	out := make([]application.Event, 0, len(snaps))
	for _, snap := range snaps {
		var v application.Event
		if e := decodeDocument(snap, "event", &v); e != nil {
			return nil, false, e
		}
		out = append(out, v)
	}
	return out, more, nil
}
func (u *unit) SaveRequirement(ctx context.Context, value domain.Requirement, expected domain.Version) error {
	ref, err := u.store.path("requirements", value.ID.String())
	if err != nil {
		return err
	}
	var old requirementRecord
	got, err := u.value(ref, "requirement", &old)
	if err != nil {
		return err
	}
	var current domain.Version
	if got {
		current = old.Requirement.Version
	}
	if err := u.adjustCounter(ctx, value.ID.String(), "requirement", string(old.Requirement.Status), string(value.Status), 0); err != nil {
		return err
	}
	return u.saveVersion(ref, "requirement", requirementRecord{Requirement: value, Text: old.Text}, expected, current)
}
func (u *unit) SaveRequirementText(ctx context.Context, id, text string) error {
	ref, err := u.store.path("requirements", id)
	if err != nil {
		return err
	}
	var current requirementRecord
	ok, err := u.value(ref, "requirement", &current)
	if err != nil {
		return err
	}
	if !ok {
		return application.ErrNotFound
	}
	current.Text = text
	return u.stage(ref, "requirement", current, false)
}
func (u *unit) RequirementText(ctx context.Context, id string) (string, bool, error) {
	ref, err := u.store.path("requirements", id)
	if err != nil {
		return "", false, err
	}
	var v requirementRecord
	ok, err := u.value(ref, "requirement", &v)
	return v.Text, ok, err
}
func (u *unit) Increment(ctx context.Context, id string) (domain.Increment, bool, error) {
	ref, err := u.store.path("increments", id)
	if err != nil {
		return domain.Increment{}, false, err
	}
	var v domain.Increment
	ok, err := u.value(ref, "increment", &v)
	return v, ok, err
}
func (u *unit) SaveIncrement(ctx context.Context, value domain.Increment, expected domain.Version) error {
	ref, err := u.store.path("increments", value.ID.String())
	if err != nil {
		return err
	}
	var old domain.Increment
	got, err := u.value(ref, "increment", &old)
	if err != nil {
		return err
	}
	var current domain.Version
	if got {
		current = old.Version
	}
	if err := u.adjustCounter(ctx, value.ID.String(), "increment", string(old.Status), string(value.Status), 0); err != nil {
		return err
	}
	return u.saveVersion(ref, "increment", value, expected, current)
}
func (u *unit) Execution(ctx context.Context, id string) (domain.Execution, bool, error) {
	ref, err := u.store.path("executions", id)
	if err != nil {
		return domain.Execution{}, false, err
	}
	var v domain.Execution
	ok, err := u.value(ref, "execution", &v)
	return v, ok, err
}
func (u *unit) SaveExecution(ctx context.Context, value domain.Execution, expected domain.Version) error {
	ref, err := u.store.path("executions", value.ID.String())
	if err != nil {
		return err
	}
	var old domain.Execution
	got, err := u.value(ref, "execution", &old)
	if err != nil {
		return err
	}
	var current domain.Version
	if got {
		current = old.Version
	}
	oldActive := 0
	if got && (old.Status == domain.ExecutionRunning || old.Status == domain.ExecutionStarting) {
		oldActive = 1
	}
	newActive := 0
	if value.Status == domain.ExecutionRunning || value.Status == domain.ExecutionStarting {
		newActive = 1
	}
	if err := u.adjustCounter(ctx, value.ID.String(), "execution", string(old.Status), string(value.Status), newActive-oldActive); err != nil {
		return err
	}
	return u.saveVersion(ref, "execution", value, expected, current)
}
func (u *unit) Lease(ctx context.Context, id string) (domain.Lease, bool, error) {
	ref, err := u.store.path("leases", id)
	if err != nil {
		return domain.Lease{}, false, err
	}
	var v domain.Lease
	ok, err := u.value(ref, "lease", &v)
	return v, ok, err
}
func (u *unit) SaveLease(ctx context.Context, value domain.Lease, expected domain.Version) error {
	ref, err := u.store.path("leases", value.ID.String())
	if err != nil {
		return err
	}
	var old domain.Lease
	got, err := u.value(ref, "lease", &old)
	if err != nil {
		return err
	}
	var current domain.Version
	if got {
		current = old.Version
	}
	return u.saveVersion(ref, "lease", value, expected, current)
}
func (u *unit) ActiveLeaseForIncrement(ctx context.Context, id string) (domain.Lease, bool, error) {
	rows, err := u.queryWhere(ctx, "leases", "lease", "increment_id", id, "status", string(domain.LeaseActive))
	if err != nil {
		return domain.Lease{}, false, err
	}
	for _, b := range rows {
		var v domain.Lease
		if json.Unmarshal(b, &v) != nil {
			return domain.Lease{}, false, ErrInvalidSchema
		}
		return v, true, nil
	}
	return domain.Lease{}, false, nil
}
func (u *unit) ActiveLeaseForIncrementAt(ctx context.Context, id string, at time.Time) (domain.Lease, bool, error) {
	v, ok, err := u.ActiveLeaseForIncrement(ctx, id)
	if err != nil || !ok {
		return v, ok, err
	}
	return v, v.ActiveAt(at), nil
}
func (u *unit) ExpiredActiveLeases(ctx context.Context, at time.Time, cursor string, limit int) ([]domain.Lease, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	path, err := CollectionPath(u.store.installation, "leases")
	if err != nil {
		return nil, "", err
	}
	q := u.store.client.Collection(path).
		Where("lease_status", "==", string(domain.LeaseActive)).
		Where("lease_expires_at", "<=", at.UTC().Format(time.RFC3339Nano)).
		OrderBy("lease_expires_at", cloudfirestore.Asc).
		OrderBy(cloudfirestore.DocumentID, cloudfirestore.Asc)
	if cursor != "" {
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(cursor)
		parts := strings.SplitN(string(decoded), "\n", 2)
		if decodeErr != nil || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, "", errors.New("invalid lease reconciliation cursor")
		}
		q = q.StartAfter(parts[0], parts[1])
	}
	snaps, err := u.tx.Documents(q.Limit(limit + 1)).GetAll()
	if err != nil {
		return nil, "", err
	}
	u.countReads(snaps)
	more := len(snaps) > limit
	if more {
		snaps = snaps[:limit]
	}
	out := make([]domain.Lease, 0, len(snaps))
	for _, snap := range snaps {
		var v domain.Lease
		if decodeDocument(snap, "lease", &v) != nil {
			return nil, "", ErrInvalidSchema
		}
		out = append(out, v)
	}
	next := ""
	if more && len(snaps) > 0 {
		last := snaps[len(snaps)-1]
		var d document
		if last.DataTo(&d) != nil || d.LeaseExpiresAt == "" {
			return nil, "", ErrInvalidSchema
		}
		next = base64.RawURLEncoding.EncodeToString([]byte(d.LeaseExpiresAt + "\n" + last.Ref.ID))
	}
	return out, next, nil
}
func (u *unit) ActiveLeases(ctx context.Context, limit int) ([]domain.Lease, error) {
	if limit <= 0 || limit > 101 {
		limit = 101
	}
	path, err := CollectionPath(u.store.installation, "leases")
	if err != nil {
		return nil, err
	}
	snaps, err := u.tx.Documents(u.store.client.Collection(path).Where("lease_status", "==", string(domain.LeaseActive)).OrderBy(cloudfirestore.DocumentID, cloudfirestore.Asc).Limit(limit + 1)).GetAll()
	if err != nil {
		return nil, err
	}
	u.countReads(snaps)
	if len(snaps) > limit {
		return nil, errors.New("active lease safety limit exceeded")
	}
	out := make([]domain.Lease, 0, len(snaps))
	for _, snap := range snaps {
		var v domain.Lease
		if decodeDocument(snap, "lease", &v) != nil {
			return nil, ErrInvalidSchema
		}
		out = append(out, v)
	}
	return out, nil
}
func (u *unit) ExecutionByLease(ctx context.Context, leaseID string) (domain.Execution, bool, error) {
	path, err := CollectionPath(u.store.installation, "executions")
	if err != nil {
		return domain.Execution{}, false, err
	}
	snaps, err := u.tx.Documents(u.store.client.Collection(path).Where("index_lease_id", "==", leaseID).Limit(2)).GetAll()
	if err != nil {
		return domain.Execution{}, false, err
	}
	u.countReads(snaps)
	if len(snaps) == 0 {
		return domain.Execution{}, false, nil
	}
	if len(snaps) != 1 {
		return domain.Execution{}, false, ErrInvalidSchema
	}
	var v domain.Execution
	if decodeDocument(snaps[0], "execution", &v) != nil {
		return domain.Execution{}, false, ErrInvalidSchema
	}
	return v, true, nil
}
func (u *unit) PendingControlProgresses(ctx context.Context, limit int) ([]domain.ControlProgress, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	path, err := CollectionPath(u.store.installation, "control_progress")
	if err != nil {
		return nil, err
	}
	snaps, err := u.tx.Documents(u.store.client.Collection(path).Where("control_verification", "==", string(domain.VerificationPending)).OrderBy(cloudfirestore.DocumentID, cloudfirestore.Asc).Limit(limit)).GetAll()
	if err != nil {
		return nil, err
	}
	u.countReads(snaps)
	out := make([]domain.ControlProgress, 0, len(snaps))
	for _, snap := range snaps {
		var v domain.ControlProgress
		if decodeDocument(snap, "control-progress", &v) != nil {
			return nil, ErrInvalidSchema
		}
		out = append(out, v)
	}
	return out, nil
}
func (u *unit) OutboxResolution(ctx context.Context, leaseID string) (application.OutboxResolution, error) {
	path, err := CollectionPath(u.store.installation, "outbox")
	if err != nil {
		return application.OutboxResolution{}, err
	}
	const limit = 100
	snaps, err := u.tx.Documents(u.store.client.Collection(path).Where("index_lease_id", "==", leaseID).Limit(limit + 1)).GetAll()
	if err != nil {
		return application.OutboxResolution{}, err
	}
	u.countReads(snaps)
	if len(snaps) > limit {
		return application.OutboxResolution{}, errors.New("outbox resolution safety limit exceeded")
	}
	var result application.OutboxResolution
	for _, snap := range snaps {
		var v application.OutboxItem
		if decodeDocument(snap, "outbox", &v) != nil {
			return application.OutboxResolution{}, ErrInvalidSchema
		}
		switch v.Status {
		case application.OutboxDelivered, application.OutboxConfirmed, application.OutboxSuperseded:
		case application.OutboxAmbiguous, application.OutboxReconciling, application.OutboxNeedsInput, application.OutboxDead:
			result.Ambiguous = true
		default:
			result.Pending = true
		}
	}
	return result, nil
}
func (u *unit) LatestLeaseForIncrement(ctx context.Context, id string) (domain.Lease, bool, error) {
	rows, err := u.queryWhere(ctx, "leases", "lease", "increment_id", id, "", "")
	if err != nil {
		return domain.Lease{}, false, err
	}
	var latest domain.Lease
	found := false
	for _, b := range rows {
		var v domain.Lease
		if json.Unmarshal(b, &v) != nil {
			return latest, false, ErrInvalidSchema
		}
		if !found || v.FencingToken > latest.FencingToken {
			latest, found = v, true
		}
	}
	return latest, found, nil
}
func (u *unit) MaxFencingToken(ctx context.Context, id string) (domain.FencingToken, error) {
	v, ok, err := u.LatestLeaseForIncrement(ctx, id)
	if err != nil || !ok {
		return 0, err
	}
	return v.FencingToken, nil
}
func (u *unit) CanonicalTarget(ctx context.Context, incrementID, runnerID string) (domain.ControlTarget, bool, error) {
	ref, err := u.store.path("targets", incrementID)
	if err != nil {
		return domain.ControlTarget{}, false, err
	}
	var v domain.ControlTarget
	ok, err := u.value(ref, "target", &v)
	return v, ok, err
}
func (u *unit) SaveCanonicalTarget(ctx context.Context, incrementID string, target domain.ControlTarget) error {
	ref, err := u.store.path("targets", incrementID)
	if err != nil {
		return err
	}
	return u.stage(ref, "target", target, false)
}
func (u *unit) Controls(ctx context.Context) ([]domain.ControlIntent, error) {
	rows, err := u.query(ctx, "controls", "control")
	if err != nil {
		return nil, err
	}
	out := make([]domain.ControlIntent, 0, len(rows))
	for _, b := range rows {
		var v domain.ControlIntent
		if json.Unmarshal(b, &v) != nil {
			return nil, ErrInvalidSchema
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Revision < out[j].Revision })
	return out, nil
}
func (u *unit) SaveControl(ctx context.Context, value domain.ControlIntent, expected domain.Revision) error {
	current, err := u.ControlRevision(ctx)
	if err != nil {
		return err
	}
	if current != expected {
		return domain.ErrStaleVersion
	}
	ref, err := u.store.path("controls", fmt.Sprintf("%d", value.Revision))
	if err != nil {
		return err
	}
	return u.stage(ref, "control", value, true)
}
func (u *unit) ControlRevision(ctx context.Context) (domain.Revision, error) {
	rows, err := u.query(ctx, "controls", "control")
	if err != nil {
		return 0, err
	}
	var current domain.Revision
	for _, b := range rows {
		var v domain.ControlIntent
		if json.Unmarshal(b, &v) != nil {
			return 0, ErrInvalidSchema
		}
		if v.Revision > current {
			current = v.Revision
		}
	}
	for _, p := range u.values {
		if p.doc.Kind != "control" {
			continue
		}
		var v domain.ControlIntent
		if json.Unmarshal([]byte(p.doc.Payload), &v) == nil && v.Revision > current {
			current = v.Revision
		}
	}
	return current, nil
}

func (u *unit) ControlProgress(ctx context.Context, revision domain.Revision) (domain.ControlProgress, bool, error) {
	ref, err := u.store.path("control_progress", fmt.Sprintf("%d", revision))
	if err != nil {
		return domain.ControlProgress{}, false, err
	}
	var v domain.ControlProgress
	ok, err := u.value(ref, "control-progress", &v)
	return v, ok, err
}
func (u *unit) SaveControlProgress(ctx context.Context, value domain.ControlProgress, expected domain.ControlState) error {
	ref, err := u.store.path("control_progress", fmt.Sprintf("%d", value.Revision))
	if err != nil {
		return err
	}
	var old domain.ControlProgress
	ok, err := u.value(ref, "control-progress", &old)
	if err != nil {
		return err
	}
	if (!ok && expected != "") || (ok && old.State != expected) {
		return domain.ErrStaleVersion
	}
	return u.stage(ref, "control-progress", value, !ok)
}
func (u *unit) ControlRequestedBy(ctx context.Context, revision domain.Revision) (domain.RequestedBy, bool, error) {
	ref, err := u.store.path("control_requested_by", fmt.Sprintf("%d", revision))
	if err != nil {
		return domain.RequestedBy{}, false, err
	}
	var v domain.RequestedBy
	ok, err := u.value(ref, "control-requested-by", &v)
	return v, ok, err
}
func (u *unit) SaveControlRequestedBy(ctx context.Context, revision domain.Revision, value domain.RequestedBy) error {
	ref, err := u.store.path("control_requested_by", fmt.Sprintf("%d", revision))
	if err != nil {
		return err
	}
	var old domain.RequestedBy
	ok, err := u.value(ref, "control-requested-by", &old)
	if err != nil {
		return err
	}
	return u.stage(ref, "control-requested-by", value, !ok)
}

// Repository and its bounded forge Observation live in their own
// collections. SaveRepository uses saveVersion, so the optimistic-concurrency
// contract is byte-for-byte the one SaveRequirement uses: a create is staged
// with Create (expected 0 and current 0) and any other save must declare the
// stored version exactly. No composite index is required: Repositories reads
// the bounded collection with no ordering or predicate, exactly as Controls
// does, so firestore.indexes.json is untouched.
func (u *unit) Repository(ctx context.Context, id string) (domain.Repository, bool, error) {
	ref, err := u.store.path("repositories", id)
	if err != nil {
		return domain.Repository{}, false, err
	}
	var v domain.Repository
	ok, err := u.value(ref, "repository", &v)
	return v, ok, err
}
func (u *unit) Repositories(ctx context.Context) ([]domain.Repository, error) {
	rows, err := u.query(ctx, "repositories", "repository")
	if err != nil {
		return nil, err
	}
	out := make([]domain.Repository, 0, len(rows))
	for _, b := range rows {
		var v domain.Repository
		if json.Unmarshal(b, &v) != nil {
			return nil, ErrInvalidSchema
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}
func (u *unit) SaveRepository(ctx context.Context, value domain.Repository, expected domain.Version) error {
	ref, err := u.store.path("repositories", value.ID.String())
	if err != nil {
		return err
	}
	var old domain.Repository
	got, err := u.value(ref, "repository", &old)
	if err != nil {
		return err
	}
	var current domain.Version
	if got {
		current = old.Version
	}
	return u.saveVersion(ref, "repository", value, expected, current)
}
func (u *unit) RepositoryObservation(ctx context.Context, repositoryID string) (domain.RepositoryObservation, bool, error) {
	ref, err := u.store.path("repository_observations", repositoryID)
	if err != nil {
		return domain.RepositoryObservation{}, false, err
	}
	var v domain.RepositoryObservation
	ok, err := u.value(ref, "repository-observation", &v)
	return v, ok, err
}
func (u *unit) SaveRepositoryObservation(ctx context.Context, value domain.RepositoryObservation) error {
	ref, err := u.store.path("repository_observations", value.RepositoryID.String())
	if err != nil {
		return err
	}
	return u.stage(ref, "repository-observation", value, false)
}

// The Requirement-to-Repository link lives in its own collection, keyed by
// the Requirement id, and is written at most once: a second link naming a
// different Repository is refused with domain.ErrStaleVersion (the same
// conflict every other save in this adapter reports) and an identical
// re-write is an idempotent replay that stages nothing.
func (u *unit) SaveRequirementRepositoryLink(ctx context.Context, value domain.RequirementRepositoryLink) error {
	if err := domain.ValidateRequirementRepositoryLink(value); err != nil {
		return err
	}
	ref, err := u.store.path("requirement_repository_links", value.RequirementID.String())
	if err != nil {
		return err
	}
	var old domain.RequirementRepositoryLink
	got, err := u.value(ref, "requirement-repository-link", &old)
	if err != nil {
		return err
	}
	if got {
		if old.RepositoryID != value.RepositoryID {
			return domain.ErrStaleVersion
		}
		return nil
	}
	return u.stage(ref, "requirement-repository-link", value, true)
}
func (u *unit) RequirementRepositoryLink(ctx context.Context, requirementID string) (domain.RequirementRepositoryLink, bool, error) {
	ref, err := u.store.path("requirement_repository_links", requirementID)
	if err != nil {
		return domain.RequirementRepositoryLink{}, false, err
	}
	var v domain.RequirementRepositoryLink
	ok, err := u.value(ref, "requirement-repository-link", &v)
	return v, ok, err
}

// RequirementRepositoryLinks reads exactly the ids on the caller's page: one
// bounded document read per id rather than a collection scan, so the bound is
// the caller's own list. Reading each key directly also means a link staged
// earlier in this same transaction is visible, which a query would not be.
func (u *unit) RequirementRepositoryLinks(ctx context.Context, ids []string) (map[string]domain.RequirementRepositoryLink, error) {
	out := make(map[string]domain.RequirementRepositoryLink, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	if len(ids) > MaxQueryRows {
		return nil, ErrQueryLimit
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		v, ok, err := u.RequirementRepositoryLink(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			out[id] = v
		}
	}
	return out, nil
}

// RequirementIDsForRepository applies its bound in the storage query, in the
// same Where(...).Limit(n+1) shape ExecutionByLease and OutboxResolution
// already use for index_lease_id. The extra row is what distinguishes "this
// is the whole answer" from "the answer was truncated", which the bool
// reports so no caller can present a bounded count as an exact total.
func (u *unit) RequirementIDsForRepository(ctx context.Context, repositoryID string, limit int) ([]string, bool, error) {
	if repositoryID == "" {
		return nil, false, errors.New("repository id is required")
	}
	if limit <= 0 {
		return nil, false, nil
	}
	if limit > MaxQueryRows {
		limit = MaxQueryRows
	}
	path, err := CollectionPath(u.store.installation, "requirement_repository_links")
	if err != nil {
		return nil, false, err
	}
	q := u.store.client.Collection(path).
		Where("index_repository_id", "==", repositoryID).
		OrderBy(cloudfirestore.DocumentID, cloudfirestore.Asc).
		Limit(limit + 1)
	snaps, err := u.tx.Documents(q).GetAll()
	if err != nil {
		return nil, false, err
	}
	u.countReads(snaps)
	truncated := len(snaps) > limit
	if truncated {
		snaps = snaps[:limit]
	}
	out := make([]string, 0, len(snaps))
	for _, snap := range snaps {
		var v domain.RequirementRepositoryLink
		if decodeDocument(snap, "requirement-repository-link", &v) != nil {
			return nil, false, ErrInvalidSchema
		}
		out = append(out, v.RequirementID.String())
	}
	sort.Strings(out)
	return out, truncated, nil
}

func (u *unit) RunnerObservation(ctx context.Context, runnerID string) (domain.RunnerObservation, bool, error) {
	ref, err := u.store.path("runner_observations", runnerID)
	if err != nil {
		return domain.RunnerObservation{}, false, err
	}
	var v domain.RunnerObservation
	ok, err := u.value(ref, "runner-observation", &v)
	return v, ok, err
}
func (u *unit) SaveRunnerObservation(ctx context.Context, value domain.RunnerObservation) error {
	ref, err := u.store.path("runner_observations", value.RunnerID.String())
	if err != nil {
		return err
	}
	return u.stage(ref, "runner-observation", value, false)
}
func (u *unit) Idempotency(ctx context.Context, requestID, operation string) (application.IdempotentResponse, bool, error) {
	ref, err := u.store.path("idempotency", requestID)
	if err != nil {
		return application.IdempotentResponse{}, false, err
	}
	var v application.IdempotentResponse
	ok, err := u.value(ref, "idempotency", &v)
	if err != nil || !ok {
		return v, ok, err
	}
	if v.Operation != operation {
		return v, true, application.ErrIdempotencyConflict
	}
	return v, true, nil
}
func (u *unit) SaveIdempotency(ctx context.Context, value application.IdempotentResponse) error {
	ref, err := u.store.path("idempotency", value.RequestID)
	if err != nil {
		return err
	}
	var old application.IdempotentResponse
	got, err := u.value(ref, "idempotency", &old)
	if err != nil {
		return err
	}
	if got && (old.Operation != value.Operation || old.Fingerprint != value.Fingerprint) {
		return application.ErrIdempotencyConflict
	}
	if got {
		return nil
	}
	return u.stage(ref, "idempotency", value, true)
}

func (u *unit) Outbox(ctx context.Context, id string) (application.OutboxItem, bool, error) {
	ref, err := u.store.path("outbox", id)
	if err != nil {
		return application.OutboxItem{}, false, err
	}
	var v application.OutboxItem
	ok, err := u.value(ref, "outbox", &v)
	if err != nil || !ok {
		return v, ok, err
	}
	if !v.Status.Valid() {
		return application.OutboxItem{}, false, application.ErrInvalidOutbox
	}
	if v.Status == "" {
		v.Status = application.OutboxPending
	}
	if v.Version == 0 {
		v.Version = 1
	}
	return v, true, nil
}

func (u *unit) Outboxes(ctx context.Context, now time.Time, limit int) ([]application.OutboxItem, error) {
	if limit <= 0 {
		return nil, nil
	}
	path, err := CollectionPath(u.store.installation, "outbox")
	if err != nil {
		return nil, err
	}
	// The projection fields are indexed separately from the JSON payload. This
	// keeps candidate reads bounded even when the payload itself grows, while
	// retaining the payload envelope as the canonical record.
	query := u.store.client.Collection(path).
		Where("outbox_status", "in", []string{string(application.OutboxPending), string(application.OutboxWaiting), string(application.OutboxDelivering)}).
		OrderBy("outbox_status", cloudfirestore.Asc).
		OrderBy("outbox_next_attempt_at", cloudfirestore.Asc).
		Limit(MaxQueryRows + 1)
	snaps, err := u.tx.Documents(query).GetAll()
	if err != nil {
		return nil, err
	}
	u.countReads(snaps)
	if len(snaps) > MaxQueryRows {
		return nil, ErrQueryLimit
	}
	seen := map[string]bool{}
	rows := make([]scannedRow, 0, len(snaps))
	for _, snap := range snaps {
		seen[snap.Ref.Path] = true
		var d document
		if snap.DataTo(&d) != nil || !RecordSchemaAccepted(d.RecordSchema) || d.Kind != "outbox" {
			return nil, ErrInvalidSchema
		}
		rows = append(rows, scannedRow{envelope: d.RecordSchema, payload: []byte(d.Payload)})
	}
	// Preserve read-your-writes for an outbox staged in this transaction.
	for p, v := range u.values {
		if v.doc.Kind == "outbox" && !seen[p] {
			rows = append(rows, scannedRow{envelope: v.doc.RecordSchema, payload: []byte(v.doc.Payload)})
		}
	}
	out := make([]application.OutboxItem, 0, len(rows))
	for _, row := range rows {
		var v application.OutboxItem
		if decodePayload(row.envelope, row.payload, &v) != nil {
			return nil, ErrInvalidSchema
		}
		if !v.Status.Valid() {
			return nil, application.ErrInvalidOutbox
		}
		if v.Status == "" {
			v.Status = application.OutboxPending
		}
		if v.Version == 0 {
			v.Version = 1
		}
		ready := v.Status == application.OutboxPending || v.Status == application.OutboxAmbiguous || v.Status == application.OutboxReconciling || (v.Status == application.OutboxWaiting && (v.NextAttemptAt.IsZero() || !v.NextAttemptAt.After(now))) || (v.Status == application.OutboxDelivering && !v.DeliveryLeaseUntil.IsZero() && !v.DeliveryLeaseUntil.After(now))
		if ready {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (u *unit) SaveOutbox(ctx context.Context, value application.OutboxItem, expected domain.Version) error {
	if value.ID == "" || !value.Status.Valid() {
		return application.ErrInvalidOutbox
	}
	if value.Status == "" {
		value.Status = application.OutboxPending
	}
	ref, err := u.store.path("outbox", value.ID)
	if err != nil {
		return err
	}
	var old application.OutboxItem
	got, err := u.value(ref, "outbox", &old)
	if err != nil {
		return err
	}
	current := domain.Version(0)
	if got {
		current = old.Version
		if current == 0 {
			current = 1
		}
	}
	if current != expected {
		return domain.ErrStaleVersion
	}
	value.Version = current + 1
	return u.stage(ref, "outbox", value, !got)
}

func (u *unit) Record(event application.Event, outbox *application.OutboxItem) error {
	ref, err := u.store.path("events", event.ID)
	if err != nil {
		return err
	}
	if err = u.stage(ref, "event", event, true); err != nil {
		return err
	}
	if outbox != nil {
		ref, err = u.store.path("outbox", outbox.ID)
		if err != nil {
			return err
		}
		item := *outbox
		if item.Status == "" {
			item.Status = application.OutboxPending
		}
		if item.Version == 0 {
			item.Version = 1
		}
		if err = u.stage(ref, "outbox", item, true); err != nil {
			return err
		}
	}
	return nil
}

func (u *unit) query(ctx context.Context, collection, kind string) ([][]byte, error) {
	path, err := CollectionPath(u.store.installation, collection)
	if err != nil {
		return nil, err
	}
	snaps, err := u.tx.Documents(u.store.client.Collection(path).Limit(MaxQueryRows + 1)).GetAll()
	if err != nil {
		return nil, err
	}
	u.countReads(snaps)
	if len(snaps) > MaxQueryRows {
		return nil, ErrQueryLimit
	}
	seen := map[string]bool{}
	out := make([][]byte, 0, len(snaps)+len(u.values))
	for _, snap := range snaps {
		seen[snap.Ref.Path] = true
		var d document
		if snap.DataTo(&d) != nil || !RecordSchemaAccepted(d.RecordSchema) || d.Kind != kind {
			return nil, ErrInvalidSchema
		}
		if err := requireInterpretableScannedPayload(d.RecordSchema, kind, []byte(d.Payload)); err != nil {
			return nil, err
		}
		out = append(out, []byte(d.Payload))
	}
	for p, v := range u.values {
		if v.doc.Kind == kind && !seen[p] {
			out = append(out, []byte(v.doc.Payload))
		}
	}
	return out, nil
}

func (u *unit) adjustCounter(ctx context.Context, id, kind, oldStatus, newStatus string, activeDelta int) error {
	shard := int(crc32.ChecksumIEEE([]byte(id)) % queueShards)
	ref, err := u.store.path("queue_counters", fmt.Sprintf("%02d", shard))
	if err != nil {
		return err
	}
	var c queueCounter
	ok, err := u.value(ref, "queue-counter", &c)
	if err != nil {
		return err
	}
	if !ok {
		c = queueCounter{Schema: "v1", Requirements: map[string]int{}, Increments: map[string]int{}}
	}
	if kind == "requirement" {
		if oldStatus != "" {
			c.Requirements[oldStatus]--
		}
		c.Requirements[newStatus]++
	}
	if kind == "increment" {
		if oldStatus != "" {
			c.Increments[oldStatus]--
		}
		c.Increments[newStatus]++
	}
	c.ActiveExecutions += activeDelta
	return u.stage(ref, "queue-counter", c, !ok)
}
func (u *unit) QueueSummary(ctx context.Context) (application.QueueSummary, error) {
	refs := make([]*cloudfirestore.DocumentRef, queueShards)
	for i := range refs {
		r, e := u.store.path("queue_counters", fmt.Sprintf("%02d", i))
		if e != nil {
			return application.QueueSummary{}, e
		}
		refs[i] = r
	}
	snaps, e := u.tx.GetAll(refs)
	if e != nil {
		return application.QueueSummary{}, e
	}
	u.countReads(snaps)
	out := application.QueueSummary{ByRequirementStatus: map[string]int{}, ByIncrementStatus: map[string]int{}}
	for _, snap := range snaps {
		if snap == nil || !snap.Exists() {
			continue
		}
		var c queueCounter
		if e := decodeDocument(snap, "queue-counter", &c); e != nil {
			return out, e
		}
		for k, v := range c.Requirements {
			out.ByRequirementStatus[k] += v
			out.Requirements += v
		}
		for k, v := range c.Increments {
			out.ByIncrementStatus[k] += v
			out.Increments += v
		}
		out.ActiveExecutions += c.ActiveExecutions
	}
	return out, nil
}
func (u *unit) queryWhere(ctx context.Context, collection, kind, field, value, field2, value2 string) ([][]byte, error) {
	// Domain structs intentionally use opaque Go types and do not expose a
	// Firestore field-name contract. Querying those field names would silently
	// break when a codec changes, so this bounded adapter reads the collection
	// and applies the predicate to decoded values. M2-B adds indexed candidate
	// projections once their schema is public.
	rows, err := u.query(ctx, collection, kind)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(rows))
	for _, b := range rows {
		var lease domain.Lease
		if kind != "lease" || json.Unmarshal(b, &lease) != nil {
			return nil, ErrInvalidSchema
		}
		match := func(name, expected string) bool {
			switch name {
			case "increment_id":
				return lease.IncrementID.String() == expected
			case "status":
				return string(lease.Status) == expected
			}
			return false
		}
		if !match(field, value) || (field2 != "" && !match(field2, value2)) {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

func (s *Store) Events(ctx context.Context) ([]application.Event, error) {
	return readCollection[application.Event](ctx, s, "events", "event")
}
func (s *Store) Outbox(ctx context.Context) ([]application.OutboxItem, error) {
	return readCollection[application.OutboxItem](ctx, s, "outbox", "outbox")
}
func readCollection[T any](ctx context.Context, s *Store, collection, kind string) ([]T, error) {
	path, err := CollectionPath(s.installation, collection)
	if err != nil {
		return nil, err
	}
	it := s.client.Collection(path).Limit(MaxQueryRows + 1).Documents(ctx)
	defer it.Stop()
	out := []T{}
	for {
		snap, e := it.Next()
		if e == iterator.Done {
			break
		}
		if e != nil {
			return nil, e
		}
		if len(out) >= MaxQueryRows {
			return nil, ErrQueryLimit
		}
		var d document
		if e = snap.DataTo(&d); e != nil || !RecordSchemaAccepted(d.RecordSchema) || d.Kind != kind {
			return nil, ErrInvalidSchema
		}
		var v T
		if e = decodePayload(d.RecordSchema, []byte(d.Payload), &v); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}
