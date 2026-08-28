package provider

// Provider account pool (V2-027 A9, dp-v2-027 d8).
//
// The pool is a closed, three-slot registry keyed by adapter name, with
// exactly one slot per Provider. That shape is measured, not assumed: the
// standing authorization authorizes codex, claude and opencode as one
// enum-constrained set and the subscription is one identity per CLI, so the
// pool is a selector over Providers and not over accounts of one Provider.
// The per-Provider concurrency ceiling is therefore one by construction:
// Acquire is the only function in this package that returns a Lease, and it
// refuses while a slot is already in use, so no code path can hand out two
// live leases for one name. Two concurrent invocations on one subscription
// would share one usage window, so a pool that handed out two leases would
// be claiming an isolation that does not exist.
//
// A slot deliberately holds no credential value, no environment value, no
// authentication token, no session identifier, no owner identity, no
// executable path and no threshold number:
//
//   - the credential already has exactly one home, the runner's Secret
//     Broker, so a second home here would be a second place for it to leak
//     from. HISTORICAL MEASUREMENT, 2026-08-25 (V2-077): this clause named
//     "the runner's Grant.Apply" as "the only path that merges a value into
//     an Invocation environment". CURRENT MEASUREMENT, 2026-08-26 (V2-078):
//     that path never reached a process and has been deleted along with
//     Invocation's Environment field, so there is no merge path at all;
//     internal/runner builds the child's environment from the approved
//     runtime invocation policy alone. The clause's conclusion is unchanged
//     -- a slot still holds no credential value -- but it no longer rests on
//     a channel that does not exist;
//   - the runtime invocation policy carries the executable path
//     bound by its approval digest, and the runner's cost ledger refuses an
//     invocation whose resolved argv[0] disagrees with it, so a copy here
//     could silently diverge from the approved one. The slot therefore names
//     the record by id and copies nothing out of it.
//
// Every transition whose meaning depends on time -- Acquire, Release,
// StartCooldown, EndCooldown -- takes the caller's time as an argument, and
// the package reads no clock in any non-test file. That is what makes a
// cooldown reproducible. The transitions whose meaning does not depend on
// time (MarkFailure, Quarantine, StopForInspection, MarkUnauthenticated,
// Clear, IssueProbe) deliberately do not take one, so no reader can mistake
// an ignored argument for a recorded timestamp.

import (
	"errors"
	"time"
)

// SlotState is the closed set of states one pool slot may be in.
type SlotState string

const (
	// SlotAvailable means the Loop may acquire this slot now.
	SlotAvailable SlotState = "available"
	// SlotInUse means a lease for this slot is outstanding.
	SlotInUse SlotState = "in-use"
	// SlotCoolingDown means the breaker opened and set a deadline.
	SlotCoolingDown SlotState = "cooling-down"
	// SlotUnauthenticated means the required action is the owner's: either
	// the standing authorization does not cover this Provider, or the CLI
	// has no authenticated session on this machine. Nothing the Loop can do
	// moves this state, which is why the two cases share it; the slot's
	// Authorized field records which of the two it is.
	SlotUnauthenticated SlotState = "unauthenticated"
	// SlotQuarantined is where a suspected secret exposure puts a slot.
	// Clearing it is a human act and never a probe.
	SlotQuarantined SlotState = "quarantined"
	// SlotStoppedForInspection is where a reached runaway-detector limit
	// puts a slot. Reaching a limit is a stop for inspection, never a
	// success and never a failure, so it is not a Provider fault and must
	// not be reported as one.
	SlotStoppedForInspection SlotState = "stopped-for-inspection"
)

var (
	// ErrUnknownProvider is returned for any name outside the closed set.
	ErrUnknownProvider = errors.New("provider name is not one of the three adapter names")
	// ErrSlotUnavailable is returned when a slot cannot be acquired. It is
	// a refusal, never a queue: waiting for a subscription that is already
	// in use is what a second lease would silently do.
	ErrSlotUnavailable = errors.New("provider slot is not available")
	// ErrSlotNotHeld is returned when releasing a slot nobody holds.
	ErrSlotNotHeld = errors.New("provider slot is not held")
	// ErrLeaseStale is returned when a lease does not match the slot's
	// current lease serial.
	ErrLeaseStale = errors.New("provider slot lease is stale")
	// ErrProbeOutstanding is returned when a probe already exists. At most
	// one probe may be outstanding at a time, across the whole pool.
	ErrProbeOutstanding = errors.New("a probe is already outstanding")
	// ErrProbeMismatch is returned when a probe does not match the
	// outstanding one.
	ErrProbeMismatch = errors.New("probe does not match the outstanding probe")
	// ErrPoolSeedIncomplete is returned when the seed does not name exactly
	// the three adapter names. There is no fourth slot to construct.
	ErrPoolSeedIncomplete = errors.New("provider pool seed does not name exactly the three adapter names")
	// ErrCooldownNotElapsed is returned when a cooldown has not passed
	// under the caller's time argument.
	ErrCooldownNotElapsed = errors.New("provider slot cooldown has not elapsed")
	// ErrSlotNeedsExternalClearance is returned for a quarantined or
	// stopped-for-inspection slot, which no deadline and no probe can move.
	ErrSlotNeedsExternalClearance = errors.New("provider slot needs an explicit external clearance")
)

// PoolNames is the closed set of Provider names, in the documented order the
// standing authorization record's enum uses. There is no fourth name.
func PoolNames() []string { return []string{"codex", "claude", "opencode"} }

// IsProviderName reports whether name is one of the three adapter names.
func IsProviderName(name string) bool {
	for _, declared := range PoolNames() {
		if declared == name {
			return true
		}
	}
	return false
}

// SlotSeed is everything a caller may declare about one slot at
// construction. Authorized is whether the owner's standing authorization
// covers this Provider. PreflightRecordID is the id of the approved
// runtime invocation policy that governs it -- a name only. No limit,
// threshold, path or credential is accepted here, because there is no field
// to put one in.
type SlotSeed struct {
	Authorized        bool
	PreflightRecordID string
}

// Slot is the read model of one pool slot. Its field set is exactly the
// eight fields dp-v2-027 d8 lists, and a source guard asserts that a ninth
// field cannot appear without the test failing.
type Slot struct {
	Name                     string       `json:"name"`
	Authorized               bool         `json:"authorized"`
	VerifiedByLoopInvocation bool         `json:"verified_by_loop_invocation"`
	PreflightRecordID        string       `json:"preflight_record_id"`
	CooldownDeadline         time.Time    `json:"cooldown_deadline"`
	ConsecutiveFailures      int          `json:"consecutive_failures"`
	LastFailureClass         FailureClass `json:"last_failure_class"`
	State                    SlotState    `json:"state"`
}

// Lease is the only handle Acquire returns. It is a value, so holding it
// grants nothing by itself: Release checks it against the slot's current
// serial, and a stale lease cannot release a slot a later caller holds.
type Lease struct {
	Provider   string    `json:"provider"`
	Serial     uint64    `json:"serial"`
	AcquiredAt time.Time `json:"acquired_at"`
}

// Probe is the pool-issued permission to make exactly one invocation against
// an open circuit. It is not a credential and grants no access; it exists so
// that a half-open circuit cannot be probed twice concurrently.
type Probe struct {
	Provider string `json:"provider"`
	Serial   uint64 `json:"serial"`
}

// Pool is the closed three-slot Provider account pool.
type Pool struct {
	slots       map[string]*Slot
	leaseSerial map[string]uint64
	held        map[string]bool
	probe       Probe
	probeHeld   bool
	probeSerial uint64
}

// NewPool builds the pool from exactly three seeds, one per name in
// PoolNames. Any other key set is refused, so there is no fourth slot to
// construct and no way to reach one.
func NewPool(seeds map[string]SlotSeed) (*Pool, error) {
	names := PoolNames()
	if len(seeds) != len(names) {
		return nil, ErrPoolSeedIncomplete
	}
	pool := &Pool{
		slots:       make(map[string]*Slot, len(names)),
		leaseSerial: make(map[string]uint64, len(names)),
		held:        make(map[string]bool, len(names)),
	}
	for _, name := range names {
		seed, declared := seeds[name]
		if !declared {
			return nil, ErrPoolSeedIncomplete
		}
		state := SlotAvailable
		if !seed.Authorized {
			state = SlotUnauthenticated
		}
		pool.slots[name] = &Slot{
			Name:              name,
			Authorized:        seed.Authorized,
			PreflightRecordID: seed.PreflightRecordID,
			State:             state,
		}
	}
	return pool, nil
}

// Names returns the closed name set this pool covers.
func (p *Pool) Names() []string { return PoolNames() }

// Slot returns a copy of one slot's read model.
func (p *Pool) Slot(name string) (Slot, error) {
	slot, present := p.slots[name]
	if !present {
		return Slot{}, ErrUnknownProvider
	}
	return *slot, nil
}

// Acquire is the only function in this package that returns a Lease. It
// refuses -- it never queues -- when the slot is not available.
func (p *Pool) Acquire(name string, now time.Time) (Lease, error) {
	slot, present := p.slots[name]
	if !present {
		return Lease{}, ErrUnknownProvider
	}
	if slot.State != SlotAvailable || p.held[name] {
		return Lease{}, ErrSlotUnavailable
	}
	p.leaseSerial[name]++
	p.held[name] = true
	slot.State = SlotInUse
	return Lease{Provider: name, Serial: p.leaseSerial[name], AcquiredAt: now}, nil
}

// Release returns a slot the caller holds. Releasing a slot nobody holds, or
// releasing under a stale lease, is refused.
func (p *Pool) Release(lease Lease, now time.Time) error {
	slot, present := p.slots[lease.Provider]
	if !present {
		return ErrUnknownProvider
	}
	if !p.held[lease.Provider] {
		return ErrSlotNotHeld
	}
	if lease.Serial != p.leaseSerial[lease.Provider] {
		return ErrLeaseStale
	}
	p.held[lease.Provider] = false
	if slot.State == SlotInUse {
		slot.State = SlotAvailable
		if !slot.CooldownDeadline.IsZero() && now.Before(slot.CooldownDeadline) {
			slot.State = SlotCoolingDown
		}
	}
	return nil
}

// MarkSuccess records that the Loop completed an invocation through this
// slot. It is the only thing that sets VerifiedByLoopInvocation, and the
// only thing that resets the consecutive-failure counter.
func (p *Pool) MarkSuccess(name string) error {
	slot, present := p.slots[name]
	if !present {
		return ErrUnknownProvider
	}
	slot.VerifiedByLoopInvocation = true
	slot.ConsecutiveFailures = 0
	slot.LastFailureClass = ""
	if slot.State == SlotCoolingDown {
		slot.State = SlotAvailable
	}
	slot.CooldownDeadline = time.Time{}
	return nil
}

// MarkFailure records one observed failure class against the slot.
func (p *Pool) MarkFailure(name string, class FailureClass) error {
	slot, present := p.slots[name]
	if !present {
		return ErrUnknownProvider
	}
	slot.ConsecutiveFailures++
	slot.LastFailureClass = class
	return nil
}

// StartCooldown moves an available or in-use slot to cooling-down with the
// caller's deadline. It refuses to move a slot that needs an explicit
// external clearance, so an open circuit can never overwrite a quarantine.
func (p *Pool) StartCooldown(name string, deadline time.Time, now time.Time) error {
	slot, present := p.slots[name]
	if !present {
		return ErrUnknownProvider
	}
	if needsExternalClearance(slot.State) {
		return ErrSlotNeedsExternalClearance
	}
	if slot.State == SlotUnauthenticated {
		return ErrSlotNeedsExternalClearance
	}
	if !deadline.After(now) {
		return ErrCooldownNotElapsed
	}
	slot.CooldownDeadline = deadline
	slot.State = SlotCoolingDown
	return nil
}

// EndCooldown moves a cooling-down slot back to available, but only once the
// deadline has passed under the caller's time argument.
func (p *Pool) EndCooldown(name string, now time.Time) error {
	slot, present := p.slots[name]
	if !present {
		return ErrUnknownProvider
	}
	if needsExternalClearance(slot.State) || slot.State == SlotUnauthenticated {
		return ErrSlotNeedsExternalClearance
	}
	if slot.State != SlotCoolingDown {
		return ErrSlotUnavailable
	}
	if now.Before(slot.CooldownDeadline) {
		return ErrCooldownNotElapsed
	}
	slot.CooldownDeadline = time.Time{}
	slot.State = SlotAvailable
	return nil
}

// Quarantine is where a suspected secret exposure sends a slot. The required
// actions are stop, redact and revoke, all of which are human acts, so no
// deadline and no probe can move the slot afterwards.
func (p *Pool) Quarantine(name string) error {
	return p.moveToTerminalState(name, SlotQuarantined)
}

// StopForInspection is where a reached runaway-detector limit sends a slot.
// Reaching a limit is a stop for inspection, never a Provider fault.
func (p *Pool) StopForInspection(name string) error {
	return p.moveToTerminalState(name, SlotStoppedForInspection)
}

// MarkUnauthenticated is where an observed absence of a session sends a
// slot. Retrying cannot produce a session, because authenticating a CLI uses
// the owner's own identity, so the state is not one a probe may leave.
func (p *Pool) MarkUnauthenticated(name string) error {
	return p.moveToTerminalState(name, SlotUnauthenticated)
}

func (p *Pool) moveToTerminalState(name string, state SlotState) error {
	slot, present := p.slots[name]
	if !present {
		return ErrUnknownProvider
	}
	p.held[name] = false
	slot.State = state
	slot.CooldownDeadline = time.Time{}
	return nil
}

// Clear is the explicit external clearance. It is the only thing that moves
// a quarantined, stopped-for-inspection or unauthenticated slot, and it
// refuses a Provider the standing authorization does not cover.
func (p *Pool) Clear(name string) error {
	slot, present := p.slots[name]
	if !present {
		return ErrUnknownProvider
	}
	if !slot.Authorized {
		return ErrSlotNeedsExternalClearance
	}
	slot.State = SlotAvailable
	slot.CooldownDeadline = time.Time{}
	slot.ConsecutiveFailures = 0
	slot.LastFailureClass = ""
	return nil
}

func needsExternalClearance(state SlotState) bool {
	return state == SlotQuarantined || state == SlotStoppedForInspection
}

// IssueProbe issues the single outstanding probe. At most one probe exists
// at a time across the whole pool: a probe is a real invocation against a
// Provider the Loop has just decided not to send to, and issuing two at once
// would turn a bounded probe back into a retry loop.
func (p *Pool) IssueProbe(name string) (Probe, error) {
	slot, present := p.slots[name]
	if !present {
		return Probe{}, ErrUnknownProvider
	}
	if needsExternalClearance(slot.State) || slot.State == SlotUnauthenticated {
		return Probe{}, ErrSlotNeedsExternalClearance
	}
	if p.probeHeld {
		return Probe{}, ErrProbeOutstanding
	}
	p.probeSerial++
	p.probe = Probe{Provider: name, Serial: p.probeSerial}
	p.probeHeld = true
	return p.probe, nil
}

// ReturnProbe returns the outstanding probe. A probe that does not match the
// outstanding one is refused, so a stale probe cannot free the slot a later
// probe holds.
func (p *Pool) ReturnProbe(probe Probe) error {
	if !p.probeHeld {
		return ErrProbeMismatch
	}
	if probe != p.probe {
		return ErrProbeMismatch
	}
	p.probeHeld = false
	p.probe = Probe{}
	return nil
}

// ProbeOutstanding reports whether a probe is currently outstanding.
func (p *Pool) ProbeOutstanding() bool { return p.probeHeld }
