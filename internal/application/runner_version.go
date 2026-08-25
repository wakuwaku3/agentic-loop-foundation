package application

// ===========================================================================
// Runner version reporting (V2-069)
// ===========================================================================
//
// What this file is for. docs/operations/self-update.md section 6 states an
// invariant over every machine at once: the canonical schema must lie inside
// the intersection of the [schema_min, schema_max] intervals of every version
// any machine can route to. The single-machine half of that is enforced by
// internal/update. The cross-machine half needs the Control Plane to know
// which binary each Runner is actually running, and section 6 records that,
// measured, it cannot: internal/api carries a static "schema_version" and
// optimistic-concurrency Expected*Version fields and nothing else, and
// internal/domain has no Runner aggregate. This file supplies that missing
// input and nothing more.
//
// Four things this file deliberately is not.
//
//  1. It is not authority. A Runner's report is a self-claim about bytes the
//     Control Plane never receives; internal/update.Verify runs on the Runner
//     machine. No authentication, authorization or admission path reads any
//     type declared here, and no request is ever refused because of a
//     reported version. The only checks made are on shape.
//
//  2. It is not a gate. No canonical schema counter exists anywhere in the
//     repository -- currentSchema is a parameter of update.Verify,
//     update.Install and update.VerifyInstalled (internal/update/update.go
//     :127, :220, :296) and nothing owns the value -- so the intersection
//     computed here is reported and compared against nothing. Refusing an
//     advance belongs to whoever owns the advance.
//
//  3. It is not a Runner aggregate. The record is a side table keyed by
//     RunnerID, following ControlRequestedByRepository's precedent.
//     domain.RunnerObservation is rebuilt in full on every heartbeat, so a
//     version stored there would be zeroed by the next heartbeat that omitted
//     the report -- which would destroy the reported-versus-not-reported
//     distinction this whole surface exists to preserve.
//
//  4. It reports two of the five coordinate groups of self-update.md section
//     5.1 -- binary and the canonical schema interval -- and no placeholder
//     for the other three. See docs/operations/runner-version-report.md.
//
// The semver and 64-hex shapes below are re-declared here as literals rather
// than imported from internal/update: ci/components.json declares
// application's dependencies as [domain, infra, release], and internal/ci's
// manifest derivation reads test imports too, so even a test-only import of
// internal/update would move the declared component DAG, which only V2-045
// may do. The cost is that the two declarations can drift, and the only thing
// that will catch it is TestRunnerVersionShapesArePinnedToTheUpdatePatterns
// in runner_version_test.go, which pins every verdict by value.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// MaxRunnerVersionReports bounds the enumeration behind GET /v1/runners.
// Machines are not shared (self-update.md section 5.2), so the row count is
// the machine count and not a function of the Requirement count; that is why
// there is no cursor and no page_size here.
const MaxRunnerVersionReports = 32

// MaxRunnerSchemaBound is the declared ceiling on a reported schema interval
// endpoint. It bounds a self-claim so an absurd integer cannot be stored; it
// says nothing about which schema is canonical, because no canonical schema
// counter exists.
const MaxRunnerSchemaBound = 4096

// RunnerVersionReportStaleAfter is the declared staleness window. A report
// older than this is still reported, with its coordinates preserved, in state
// stale: a machine that reported and then went quiet is a different
// operational fact from one that never reported, and only the first tells an
// operator that a machine stopped heart-beating.
const RunnerVersionReportStaleAfter = 15 * time.Minute

var (
	// runnerVersionPattern is a literal copy, by value, of the pattern at
	// internal/update/update.go:64.
	runnerVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[a-z0-9.-]+)?$`)
	// runnerBinaryDigestPattern is a literal copy, by value, of the pattern
	// at internal/update/update.go:65.
	runnerBinaryDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ErrRunnerVersionReportInvalid is the shape refusal. It is a malformed
// request, never a policy decision about the Runner.
var ErrRunnerVersionReportInvalid = errors.New("runner version report is malformed")

// RunnerVersionInput is the additive optional object on the heartbeat a
// Runner already sends. It carries exactly the four reported coordinates and
// no timestamp field at all: a Runner clock is structurally unable to reach
// the record, following the rule service.go already states for process
// observations ("Runner clocks and caller-provided timestamps are not
// authoritative", service.go:450).
//
// The object is all-present or wholly absent. A partial object is refused and
// stores nothing, because an object carrying schema_max with no schema_min
// would otherwise be stored as an interval starting at zero, which reads as
// wider than anything the machine can do -- and a too-wide interval is the
// one error that makes an intersection look non-empty when it is not.
type RunnerVersionInput struct {
	Version      string
	BinarySHA256 string
	SchemaMin    int
	SchemaMax    int
}

// Validate is shape only. Nothing here checks that the claim is true; no
// control-plane code can, because the bytes it describes never arrive.
func (v RunnerVersionInput) Validate() error {
	if !runnerVersionPattern.MatchString(v.Version) {
		return fmt.Errorf("%w: version must match the module manifest semver shape", ErrRunnerVersionReportInvalid)
	}
	if !runnerBinaryDigestPattern.MatchString(v.BinarySHA256) {
		return fmt.Errorf("%w: binary_sha256 must be exactly 64 lowercase hex characters", ErrRunnerVersionReportInvalid)
	}
	if v.SchemaMin < 1 {
		return fmt.Errorf("%w: schema_min must be at least 1", ErrRunnerVersionReportInvalid)
	}
	if v.SchemaMax < v.SchemaMin {
		return fmt.Errorf("%w: schema_max must be at least schema_min", ErrRunnerVersionReportInvalid)
	}
	if v.SchemaMin > MaxRunnerSchemaBound || v.SchemaMax > MaxRunnerSchemaBound {
		return fmt.Errorf("%w: schema interval endpoints must be at most %d", ErrRunnerVersionReportInvalid, MaxRunnerSchemaBound)
	}
	return nil
}

// RunnerVersionReport is the stored record: the four reported coordinates,
// the RunnerID the authenticated session already proved, and reported_at,
// which is the transaction's authority time and never a value the Runner
// sent. There is deliberately no message, detail, output, text, hostname,
// host, ip, path, root, env or timestamp field: a machine identifier beyond
// the already-authenticated RunnerID is unrepresentable rather than filtered,
// so no redaction step can be forgotten.
type RunnerVersionReport struct {
	RunnerID     string
	Version      string
	BinarySHA256 string
	SchemaMin    int
	SchemaMax    int
	ReportedAt   time.Time
}

// Reported distinguishes a real report from a Runner that is merely known.
// An enumeration row for a Runner that has never reported carries the zero
// ReportedAt and zero coordinates; no interval is ever synthesized for it.
func (r RunnerVersionReport) Reported() bool { return !r.ReportedAt.IsZero() }

// RunnerReportState is a closed enum. not-reported and stale are first-class
// values, not shades of reported.
type RunnerReportState string

const (
	RunnerReportReported    RunnerReportState = "reported"
	RunnerReportNotReported RunnerReportState = "not-reported"
	RunnerReportStale       RunnerReportState = "stale"
)

// Valid is the closed switch. TestRunnerEnumsAreClosed parses this file and
// fails if a constant of this type has no case here, or a case here has no
// constant.
func (s RunnerReportState) Valid() bool {
	switch s {
	case RunnerReportReported, RunnerReportNotReported, RunnerReportStale:
		return true
	}
	return false
}

// SchemaIntersectionState is a closed enum whose unknown value is what makes
// self-update.md section 6's refusal sound: "an advance that would empty the
// intersection is refused, not scheduled" is a real refusal only if
// I-do-not-know cannot be silently rounded to non-empty.
type SchemaIntersectionState string

const (
	SchemaIntersectionNonEmpty SchemaIntersectionState = "non-empty"
	SchemaIntersectionEmpty    SchemaIntersectionState = "empty"
	SchemaIntersectionUnknown  SchemaIntersectionState = "unknown"
)

func (s SchemaIntersectionState) Valid() bool {
	switch s {
	case SchemaIntersectionNonEmpty, SchemaIntersectionEmpty, SchemaIntersectionUnknown:
		return true
	}
	return false
}

// RunnerVersionView is one Runner's row. The coordinates are present only
// when ReportState is reported or stale -- that is, only when a report was
// actually read. SchemaMin and SchemaMax are pointers so that "no interval at
// all" is a structural absence in the marshalled JSON rather than a zero that
// a reader could mistake for an interval starting at zero.
type RunnerVersionView struct {
	RunnerID     string            `json:"runner_id"`
	ReportState  RunnerReportState `json:"report_state"`
	Version      string            `json:"version,omitempty"`
	BinarySHA256 string            `json:"binary_sha256,omitempty"`
	SchemaMin    *int              `json:"schema_min,omitempty"`
	SchemaMax    *int              `json:"schema_max,omitempty"`
	ReportedAt   string            `json:"reported_at,omitempty"`
}

// RunnerVersionListView is the whole GET /v1/runners response. The
// intersection endpoints are present only when the state is non-empty: an
// empty or unknown intersection is not an interval and is never given
// endpoints.
type RunnerVersionListView struct {
	Runners               []RunnerVersionView     `json:"runners"`
	RunnerCount           int                     `json:"runner_count"`
	Truncated             bool                    `json:"truncated"`
	IntersectionState     SchemaIntersectionState `json:"intersection_state"`
	IntersectionSchemaMin *int                    `json:"intersection_schema_min,omitempty"`
	IntersectionSchemaMax *int                    `json:"intersection_schema_max,omitempty"`
}

func intPtr(v int) *int { return &v }

// runnerVersionView projects one stored record onto the wire shape at the
// instant now.
func runnerVersionView(report RunnerVersionReport, now time.Time) RunnerVersionView {
	if !report.Reported() {
		return RunnerVersionView{RunnerID: report.RunnerID, ReportState: RunnerReportNotReported}
	}
	state := RunnerReportReported
	if now.Sub(report.ReportedAt) > RunnerVersionReportStaleAfter {
		state = RunnerReportStale
	}
	return RunnerVersionView{
		RunnerID:     report.RunnerID,
		ReportState:  state,
		Version:      report.Version,
		BinarySHA256: report.BinarySHA256,
		SchemaMin:    intPtr(report.SchemaMin),
		SchemaMax:    intPtr(report.SchemaMax),
		ReportedAt:   report.ReportedAt.UTC().Format(time.RFC3339Nano),
	}
}

// runnerVersionListView builds the whole view, including the intersection.
//
// The intersection is unknown, never non-empty, in three cases: no Runner is
// known at all; some enumerated Runner is not in state reported; or the
// enumeration was truncated, because the invariant is a statement about every
// machine and a truncated read did not see every machine. Only when every
// enumerated Runner is in state reported are endpoints computed, as the
// maximum of the minima and the minimum of the maxima; if that pair inverts,
// the state is empty and no endpoints are reported.
func runnerVersionListView(reports []RunnerVersionReport, truncated bool, now time.Time) RunnerVersionListView {
	out := RunnerVersionListView{Runners: make([]RunnerVersionView, 0, len(reports)), Truncated: truncated}
	for _, report := range reports {
		out.Runners = append(out.Runners, runnerVersionView(report, now))
	}
	out.RunnerCount = len(out.Runners)
	out.IntersectionState = SchemaIntersectionUnknown
	if truncated || len(out.Runners) == 0 {
		return out
	}
	low, high := 0, 0
	for i, row := range out.Runners {
		if row.ReportState != RunnerReportReported {
			return out
		}
		if i == 0 || *row.SchemaMin > low {
			low = *row.SchemaMin
		}
		if i == 0 || *row.SchemaMax < high {
			high = *row.SchemaMax
		}
	}
	if low > high {
		out.IntersectionState = SchemaIntersectionEmpty
		return out
	}
	out.IntersectionState = SchemaIntersectionNonEmpty
	out.IntersectionSchemaMin = intPtr(low)
	out.IntersectionSchemaMax = intPtr(high)
	return out
}

// Runners is the owner-role read. It performs no mutation of any application
// record and enqueues no outbox item; the only document it writes is the
// per-transaction quota reservation every bounded owner read already writes.
func (s *Service) Runners(ctx context.Context) (RunnerVersionListView, error) {
	if _, _, err := callerActor(ctx, RoleOwner); err != nil {
		return RunnerVersionListView{}, err
	}
	now := s.clock.Now()
	if now.IsZero() {
		return RunnerVersionListView{}, errors.New("clock returned zero time")
	}
	var out RunnerVersionListView
	err := s.transact(ctx, func(u UnitOfWork) error {
		rows, truncated, e := u.RunnerVersionReports(ctx, MaxRunnerVersionReports)
		if e != nil {
			return e
		}
		out = runnerVersionListView(rows, truncated, now)
		return nil
	})
	return out, err
}

// transactionAuthorityTime is the same derivation Service.record uses: the
// instant captured once before the transaction callback, carried on the
// context and re-read through the unit of work so a Firestore retry of the
// callback cannot change it.
func transactionAuthorityTime(ctx context.Context, u UnitOfWork) (time.Time, error) {
	if authority, ok := u.(interface{ AuthorityContext() context.Context }); ok {
		ctx = authority.AuthorityContext()
	}
	at, ok := ctx.Value(authorityTimeKey{}).(time.Time)
	if !ok || at.IsZero() {
		return time.Time{}, errors.New("transaction authority time is required")
	}
	return at, nil
}

// ===========================================================================
// One behavioural table, two adapters
// ===========================================================================
//
// This table is data, not test machinery: it imports nothing and asserts
// nothing. It lives here so that internal/store/memory and
// internal/store/firestore run the same cases by value, and the memory store
// cannot pass behaviour the Firestore store does not implement. Each adapter's
// test supplies the driver.

// RunnerVersionReportCase is one case of that table. Reports are saved in
// order, each in its own transaction, so Want expresses last-writer-wins.
// Observations names Runners that are known to the Control Plane only through
// a heartbeat that carried no report; they must enumerate as rows whose
// ReportedAt is zero.
type RunnerVersionReportCase struct {
	Name          string
	Reports       []RunnerVersionReport
	Observations  []string
	Limit         int
	Want          []RunnerVersionReport
	WantTruncated bool
}

// RunnerVersionReportCases is the shared behavioural table.
func RunnerVersionReportCases() []RunnerVersionReportCase {
	at := time.Unix(1700000000, 0).UTC()
	later := at.Add(time.Hour)
	report := func(id, version, digest string, min, max int, when time.Time) RunnerVersionReport {
		return RunnerVersionReport{RunnerID: id, Version: version, BinarySHA256: digest, SchemaMin: min, SchemaMax: max, ReportedAt: when}
	}
	const digestA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const digestB = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

	over := make([]RunnerVersionReport, 0, MaxRunnerVersionReports+3)
	want := make([]RunnerVersionReport, 0, MaxRunnerVersionReports)
	for i := 0; i < MaxRunnerVersionReports+3; i++ {
		r := report(fmt.Sprintf("runner-%03d", i), "1.0.0", digestA, 1, 4, at)
		over = append(over, r)
		if i < MaxRunnerVersionReports {
			want = append(want, r)
		}
	}

	return []RunnerVersionReportCase{
		{
			Name:    "one report round-trips through a separate read",
			Reports: []RunnerVersionReport{report("runner-a", "1.2.3", digestA, 2, 7, at)},
			Want:    []RunnerVersionReport{report("runner-a", "1.2.3", digestA, 2, 7, at)},
		},
		{
			Name: "the last writer wins per runner id and leaves exactly one row",
			Reports: []RunnerVersionReport{
				report("runner-a", "1.2.3", digestA, 2, 7, at),
				report("runner-a", "1.3.0", digestB, 3, 9, later),
			},
			Want: []RunnerVersionReport{report("runner-a", "1.3.0", digestB, 3, 9, later)},
		},
		{
			Name:         "a runner known only through a report-less heartbeat enumerates as not reported",
			Reports:      []RunnerVersionReport{report("runner-a", "1.2.3", digestA, 2, 7, at)},
			Observations: []string{"runner-b"},
			Want: []RunnerVersionReport{
				report("runner-a", "1.2.3", digestA, 2, 7, at),
				{RunnerID: "runner-b"},
			},
		},
		{
			Name: "the order is runner id ascending, not insertion order",
			Reports: []RunnerVersionReport{
				report("runner-c", "1.0.0", digestA, 1, 4, at),
				report("runner-a", "1.0.0", digestA, 1, 4, at),
				report("runner-b", "1.0.0", digestA, 1, 4, at),
			},
			Want: []RunnerVersionReport{
				report("runner-a", "1.0.0", digestA, 1, 4, at),
				report("runner-b", "1.0.0", digestA, 1, 4, at),
				report("runner-c", "1.0.0", digestA, 1, 4, at),
			},
		},
		{
			Name:          "more reports than the bound are truncated and the truncation is reported",
			Reports:       over,
			Want:          want,
			WantTruncated: true,
		},
	}
}

// SortRunnerVersionReports puts rows in the fixed order both adapters must
// return: RunnerID ascending. Adapters use it so the order is declared in one
// place rather than twice.
func SortRunnerVersionReports(rows []RunnerVersionReport) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].RunnerID < rows[j].RunnerID })
}
