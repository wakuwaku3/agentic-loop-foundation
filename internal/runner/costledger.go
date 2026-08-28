package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CostAnomalyThresholdUSD is the single-invocation runaway-anomaly signal
// (dp-v2-017 d3/d10): a settled invocation reporting more than this halts
// the ledger permanently. It is a detector for unbounded context growth,
// never a billing rule.
const CostAnomalyThresholdUSD = 2.00

// The nine dp-v2-017 d3 fail-closed refusal reasons. Each is independently
// testable and each guarantees, structurally, that CostLedger.Reserve
// returns before any ledger mutation is persisted and before the caller may
// start a process.
var (
	ErrCostLedgerUnreadable         = errors.New("cost ledger: file exists but could not be read")
	ErrCostLedgerMalformed          = errors.New("cost ledger: file does not match the ledger's own shape")
	ErrCostLedgerRunMismatch        = errors.New("cost ledger: recorded provider or task_id does not match this run")
	ErrCostLedgerHalted             = errors.New("cost ledger: halted after a runaway-anomaly settlement; a human must clear it")
	ErrCostLedgerPreflightInvalid   = errors.New("cost ledger: approved provider preflight record is missing, schema-invalid, or fails the ledger cross-check")
	ErrCostLedgerLimitsMismatch     = errors.New("cost ledger: approved record disagrees with the ledger's recorded limits or preflight digest")
	ErrCostLedgerExecutableMissing  = errors.New("cost ledger: resolved executable path does not exist")
	ErrCostLedgerExecutableMismatch = errors.New("cost ledger: resolved executable path is not the path the approved record declares")
	ErrCostLedgerOverBudget         = errors.New("cost ledger: reservation would exceed max_invocations or max_total_cost_usd")
)

// CostLimits is the runtime invocation budget supplied by the active session.
type CostLimits struct {
	MaxInvocations          int
	MaxTotalCostUSD         float64
	WorstCaseReservationUSD float64
}

// InvocationPolicy is the bounded provider authority supplied directly to a
// Runner session. It is intentionally not loaded from a tracked handoff file.
type InvocationPolicy struct {
	ProviderName         string
	ExecutablePath       string
	Limits               CostLimits
	LedgerPath           string
	EnvironmentBaseNames []string
	EnvironmentGranted   []string
	Digest               string
}

func NewInvocationPolicy(providerName, executablePath, ledgerPath string, limits CostLimits, baseNames []string) (InvocationPolicy, error) {
	if providerName == "" || !filepath.IsAbs(executablePath) || !filepath.IsAbs(ledgerPath) || limits.MaxInvocations <= 0 || limits.MaxTotalCostUSD <= 0 || limits.WorstCaseReservationUSD <= 0 || len(baseNames) == 0 {
		return InvocationPolicy{}, ErrCostLedgerPreflightInvalid
	}
	p := InvocationPolicy{ProviderName: providerName, ExecutablePath: executablePath, LedgerPath: ledgerPath, Limits: limits, EnvironmentBaseNames: append([]string(nil), baseNames...)}
	raw, _ := json.Marshal(p)
	p.Digest = fmt.Sprintf("%x", sha256.Sum256(raw))
	return p, nil
}

// LedgerEntry is one accounted invocation. State is "reserved" until TrueUp
// settles it. ActualUSD, SessionID and FinishedAt are populated only once
// settled. ActualUSD is always the raw, truthful total_cost_usd the
// provider reported -- it is never clamped -- so an anomalous overrun above
// CostAnomalyThresholdUSD remains visible in the ledger and in evidence
// exactly as reported; only the *budget accounting* in Reserve's formula
// clamps a settled entry's contribution at its own ReservedUSD (dp-v2-017
// d2's "never raise" promise is about future budget headroom, not about
// concealing a true overrun).
//
// InputCount/OutputCount/CacheReadCount/CacheCreationCount/DurationAPIMS/
// DurationMS/NumTurns are additive usage/telemetry fields the CLI reports
// per invocation (dp-v2-017 B12); they are deliberately named as counts,
// never as "*_tokens", to keep the word "token" out of every ledger and
// evidence summary.
type LedgerEntry struct {
	Sequence           int       `json:"sequence"`
	Purpose            string    `json:"purpose"`
	State              string    `json:"state"`
	ReservedUSD        float64   `json:"reserved_usd"`
	ActualUSD          float64   `json:"actual_usd,omitempty"`
	SessionID          string    `json:"session_id,omitempty"`
	InputCount         int64     `json:"input_count,omitempty"`
	OutputCount        int64     `json:"output_count,omitempty"`
	CacheReadCount     int64     `json:"cache_read_count,omitempty"`
	CacheCreationCount int64     `json:"cache_creation_count,omitempty"`
	DurationAPIMS      int64     `json:"duration_api_ms,omitempty"`
	DurationMS         int64     `json:"duration_ms,omitempty"`
	NumTurns           int       `json:"num_turns,omitempty"`
	StartedAt          time.Time `json:"started_at"`
	FinishedAt         time.Time `json:"finished_at,omitempty"`
}

type ledgerFileLimits struct {
	MaxInvocations          int     `json:"max_invocations"`
	MaxTotalCostUSD         float64 `json:"max_total_cost_usd"`
	WorstCaseReservationUSD float64 `json:"worst_case_reservation_usd"`
}

// ledgerFile is the on-disk shape at CostLedger.Path (dp-v2-017 d4).
type ledgerFile struct {
	SchemaVersion   string           `json:"schema_version"`
	Provider        string           `json:"provider"`
	TaskID          string           `json:"task_id"`
	PreflightDigest string           `json:"preflight_digest"`
	Limits          ledgerFileLimits `json:"limits"`
	Halted          bool             `json:"halted"`
	Entries         []LedgerEntry    `json:"entries"`
}

// CostLedger is a fail-closed, file-backed runaway-detection usage ledger
// (dp-v2-017 d2/d3/d4). It is a required, non-optional dependency of
// SupervisedInvocationRunner: there is no way to construct a runner that can
// start a process without one.
type CostLedger struct {
	// Path is the absolute path declared by the approved record's
	// limits.ledger_path. It lives outside the repository and outside every
	// workspace.
	Path string
	// Provider and TaskID identify this ledger's owning run; Reserve
	// refuses if a pre-existing ledger file at Path disagrees with either.
	Provider string
	TaskID   string
}

func (c *CostLedger) readOrInit(record InvocationPolicy) (ledgerFile, error) {
	b, err := os.ReadFile(c.Path)
	if errors.Is(err, os.ErrNotExist) {
		return ledgerFile{
			SchemaVersion:   "v1",
			Provider:        c.Provider,
			TaskID:          c.TaskID,
			PreflightDigest: record.Digest,
			Limits: ledgerFileLimits{
				MaxInvocations:          record.Limits.MaxInvocations,
				MaxTotalCostUSD:         record.Limits.MaxTotalCostUSD,
				WorstCaseReservationUSD: record.Limits.WorstCaseReservationUSD,
			},
		}, nil
	}
	if err != nil {
		return ledgerFile{}, fmt.Errorf("%w: %v", ErrCostLedgerUnreadable, err)
	}
	var lf ledgerFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if decErr := dec.Decode(&lf); decErr != nil {
		return ledgerFile{}, fmt.Errorf("%w: %v", ErrCostLedgerMalformed, decErr)
	}
	if lf.SchemaVersion != "v1" {
		return ledgerFile{}, fmt.Errorf("%w: unexpected schema_version %q", ErrCostLedgerMalformed, lf.SchemaVersion)
	}
	return lf, nil
}

// writeAtomic persists lf to path via temp-file + fsync + rename, so a
// crash mid-write never leaves a torn ledger file (dp-v2-017 d2/d4).
func writeLedgerAtomic(path string, lf ledgerFile) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	b, err := json.Marshal(lf)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cost-ledger-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Reserve admits one invocation's worth of worst-case budget before any
// process may start (dp-v2-017 d1 step 2, d2, d3). record must have just
// was supplied directly by the active session. expectedArgv0 is the adapter's
// own argv[0] (e.g. "claude"); Reserve refuses unless
// filepath.Base(record.ExecutablePath) == expectedArgv0. On success it
// returns the new entry's sequence number, having already fsynced the
// updated ledger file to disk.
func (c *CostLedger) Reserve(record InvocationPolicy, expectedArgv0, purpose string, now time.Time) (int, error) {
	if c == nil || c.Path == "" || c.Provider == "" || c.TaskID == "" {
		return 0, errors.New("cost ledger is not configured")
	}
	if purpose == "" {
		return 0, errors.New("cost ledger: purpose is required")
	}
	if record.ProviderName == "" || record.ExecutablePath == "" {
		return 0, ErrCostLedgerPreflightInvalid
	}
	if record.ProviderName != c.Provider {
		return 0, fmt.Errorf("%w: record provider %q != ledger provider %q", ErrCostLedgerRunMismatch, record.ProviderName, c.Provider)
	}

	// Cases 7 and 8: executable existence and basename match, checked
	// before the ledger file is ever touched so a bad record never mutates
	// accounting.
	info, statErr := os.Stat(record.ExecutablePath)
	if statErr != nil {
		return 0, fmt.Errorf("%w: %v", ErrCostLedgerExecutableMissing, statErr)
	}
	if info.IsDir() {
		return 0, fmt.Errorf("%w: is a directory", ErrCostLedgerExecutableMissing)
	}
	if filepath.Base(record.ExecutablePath) != expectedArgv0 {
		return 0, fmt.Errorf("%w: basename(%s) != %s", ErrCostLedgerExecutableMismatch, record.ExecutablePath, expectedArgv0)
	}

	lf, err := c.readOrInit(record)
	if err != nil {
		return 0, err // already one of: ErrCostLedgerUnreadable, ErrCostLedgerMalformed
	}

	// Case 4: halted is checked before anything else that could look like
	// a routine refusal, because halted must never be recoverable by
	// chance (e.g. a lower purpose count later).
	if lf.Halted {
		return 0, ErrCostLedgerHalted
	}
	// Case 3.
	if lf.Provider != c.Provider || lf.TaskID != c.TaskID {
		return 0, fmt.Errorf("%w: ledger provider=%q task_id=%q, run provider=%q task_id=%q", ErrCostLedgerRunMismatch, lf.Provider, lf.TaskID, c.Provider, c.TaskID)
	}
	// Case 6: the approved record's limits (and its own digest) must agree
	// with what the ledger recorded when it was initialised. preflight_digest
	// is re-verified on every Reserve (dp-v2-017 d4): editing the approved
	// record after the fact invalidates the ledger instead of raising the
	// cap.
	if lf.Limits.MaxInvocations != record.Limits.MaxInvocations ||
		lf.Limits.MaxTotalCostUSD != record.Limits.MaxTotalCostUSD ||
		lf.Limits.WorstCaseReservationUSD != record.Limits.WorstCaseReservationUSD ||
		lf.PreflightDigest != record.Digest {
		return 0, fmt.Errorf("%w", ErrCostLedgerLimitsMismatch)
	}

	// Case 9: the dp-v2-017 d2 formula. total invocation count (settled +
	// reserved) is what the 16-invocation ceiling counts, per dp-v2-017 d9's
	// worked arithmetic ("once settled... the sixteen-invocation count
	// ceiling is what binds").
	// Budget accounting clamps each settled entry's contribution at its own
	// ReservedUSD (dp-v2-017 d2's "never raise" promise applies to future
	// budget headroom): a settled entry that truthfully overran its
	// reservation cannot further shrink the room left for later
	// invocations beyond what its reservation already blocked, even though
	// LedgerEntry.ActualUSD itself always stores the true, unclamped
	// figure for anomaly detection and evidence.
	var settledUSD, reservedUSD float64
	for _, e := range lf.Entries {
		switch e.State {
		case "settled":
			contribution := e.ActualUSD
			if contribution > e.ReservedUSD {
				contribution = e.ReservedUSD
			}
			settledUSD += contribution
		case "reserved":
			reservedUSD += e.ReservedUSD
		}
	}
	if len(lf.Entries)+1 > record.Limits.MaxInvocations {
		return 0, fmt.Errorf("%w: max_invocations %d already reached", ErrCostLedgerOverBudget, record.Limits.MaxInvocations)
	}
	if settledUSD+reservedUSD+record.Limits.WorstCaseReservationUSD > record.Limits.MaxTotalCostUSD {
		return 0, fmt.Errorf("%w: settled=%.4f reserved=%.4f worst_case=%.4f max=%.4f", ErrCostLedgerOverBudget, settledUSD, reservedUSD, record.Limits.WorstCaseReservationUSD, record.Limits.MaxTotalCostUSD)
	}

	seq := len(lf.Entries) + 1
	lf.Entries = append(lf.Entries, LedgerEntry{
		Sequence:    seq,
		Purpose:     purpose,
		State:       "reserved",
		ReservedUSD: record.Limits.WorstCaseReservationUSD,
		StartedAt:   now,
	})
	if err := writeLedgerAtomic(c.Path, lf); err != nil {
		return 0, err
	}
	return seq, nil
}

// ErrCostLedgerEntryNotReserved is returned by TrueUp when sequence does not
// name a currently-reserved entry.
var ErrCostLedgerEntryNotReserved = errors.New("cost ledger: sequence does not name a reserved entry")

// ErrCostLedgerTrueUpExceedsReservation is returned by TrueUp when
// settlement.ActualUSD is negative, which can never be a truthful
// total_cost_usd.
var ErrCostLedgerTrueUpExceedsReservation = errors.New("cost ledger: true-up amount is invalid")

// Settlement is the real, post-invocation figures TrueUp records against a
// previously-reserved entry (dp-v2-017 B12): the provider's own reported
// cost and usage/duration/turn telemetry. Usage is expressed as counts
// (input/output/cache-read/cache-creation), never as "*_tokens", so no
// ledger or evidence text contains the substring "token".
type Settlement struct {
	ActualUSD          float64
	SessionID          string
	InputCount         int64
	OutputCount        int64
	CacheReadCount     int64
	CacheCreationCount int64
	DurationAPIMS      int64
	DurationMS         int64
	NumTurns           int
}

// TrueUp settles a previously-reserved entry to its real cost and usage
// (dp-v2-017 d1 step 4, d2, B5, B12). ActualUSD is stored verbatim and
// truthfully -- never clamped -- so a genuine anomalous overrun stays
// visible; the "only ever lowers, never raises" promise instead governs how
// Reserve's own budget formula accounts a settled entry (clamped at its own
// ReservedUSD there, not here). TrueUp itself rejects only a structurally
// invalid settlement (a negative ActualUSD, which can never be a truthful
// total_cost_usd) or a sequence that does not name a currently-reserved
// entry.
//
// If ActualUSD exceeds CostAnomalyThresholdUSD, the ledger's halted flag is
// set atomically in the same write and every later Reserve then fails
// closed regardless of remaining budget (dp-v2-017 d3); TrueUp still
// succeeds in recording the settlement (the caller must stop making further
// invocations, but the true figure that triggered the halt must not be
// lost), and returns a distinct, non-nil error alongside the recorded
// settlement so the caller cannot mistake this for an ordinary success.
func (c *CostLedger) TrueUp(sequence int, settlement Settlement, finishedAt time.Time) error {
	if c == nil || c.Path == "" {
		return errors.New("cost ledger is not configured")
	}
	if settlement.ActualUSD < 0 {
		return fmt.Errorf("%w: actual_usd must not be negative", ErrCostLedgerTrueUpExceedsReservation)
	}
	b, err := os.ReadFile(c.Path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCostLedgerUnreadable, err)
	}
	var lf ledgerFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&lf); err != nil {
		return fmt.Errorf("%w: %v", ErrCostLedgerMalformed, err)
	}
	found := false
	anomaly := settlement.ActualUSD > CostAnomalyThresholdUSD
	for i := range lf.Entries {
		if lf.Entries[i].Sequence != sequence {
			continue
		}
		if lf.Entries[i].State != "reserved" {
			return ErrCostLedgerEntryNotReserved
		}
		lf.Entries[i].State = "settled"
		lf.Entries[i].ActualUSD = settlement.ActualUSD
		lf.Entries[i].SessionID = settlement.SessionID
		lf.Entries[i].InputCount = settlement.InputCount
		lf.Entries[i].OutputCount = settlement.OutputCount
		lf.Entries[i].CacheReadCount = settlement.CacheReadCount
		lf.Entries[i].CacheCreationCount = settlement.CacheCreationCount
		lf.Entries[i].DurationAPIMS = settlement.DurationAPIMS
		lf.Entries[i].DurationMS = settlement.DurationMS
		lf.Entries[i].NumTurns = settlement.NumTurns
		lf.Entries[i].FinishedAt = finishedAt
		found = true
		break
	}
	if !found {
		return ErrCostLedgerEntryNotReserved
	}
	if anomaly {
		lf.Halted = true
	}
	if err := writeLedgerAtomic(c.Path, lf); err != nil {
		return err
	}
	if anomaly {
		return fmt.Errorf("cost ledger: settled invocation %.4f USD exceeds the %.2f USD single-invocation anomaly threshold; halted", settlement.ActualUSD, CostAnomalyThresholdUSD)
	}
	return nil
}

// LedgerSnapshotEntry is the redacted, evidence-safe projection of one
// LedgerEntry: it carries no prompt, no response text and nothing beyond
// what dp-v2-017 B18/B20 require.
type LedgerSnapshotEntry = LedgerEntry

// LedgerSnapshot is CostLedger's read-only view for evidence recording
// (dp-v2-017 B18): every entry, plus the halted flag and the settled and
// outstanding-reserved totals.
type LedgerSnapshot struct {
	Provider         string
	TaskID           string
	Halted           bool
	SettledTotalUSD  float64
	ReservedTotalUSD float64
	InvocationCount  int
	Entries          []LedgerSnapshotEntry
}

// Snapshot reads the ledger file fresh from disk and returns its full,
// redacted accounting state. It never mutates the file.
func (c *CostLedger) Snapshot() (LedgerSnapshot, error) {
	if c == nil || c.Path == "" {
		return LedgerSnapshot{}, errors.New("cost ledger is not configured")
	}
	b, err := os.ReadFile(c.Path)
	if errors.Is(err, os.ErrNotExist) {
		return LedgerSnapshot{Provider: c.Provider, TaskID: c.TaskID}, nil
	}
	if err != nil {
		return LedgerSnapshot{}, fmt.Errorf("%w: %v", ErrCostLedgerUnreadable, err)
	}
	var lf ledgerFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&lf); err != nil {
		return LedgerSnapshot{}, fmt.Errorf("%w: %v", ErrCostLedgerMalformed, err)
	}
	snap := LedgerSnapshot{Provider: lf.Provider, TaskID: lf.TaskID, Halted: lf.Halted, InvocationCount: len(lf.Entries), Entries: append([]LedgerEntry(nil), lf.Entries...)}
	for _, e := range lf.Entries {
		switch e.State {
		case "settled":
			snap.SettledTotalUSD += e.ActualUSD
		case "reserved":
			snap.ReservedTotalUSD += e.ReservedUSD
		}
	}
	return snap, nil
}
