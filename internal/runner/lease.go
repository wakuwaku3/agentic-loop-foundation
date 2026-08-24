package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// LeaseKeeper renews a runner-held lease and reports a heartbeat as two
// separate application operations (dp-v2-016 d10). It is driven only by
// explicit Tick calls over whatever clock the caller's application.Service
// was built with; it never starts a background timer or goroutine of its
// own, so a test can advance a lease past its TTL deterministically between
// two Tick calls.
type LeaseKeeper struct {
	Service     *application.Service
	LeaseID     string
	RequestBase string // stable id namespace; each Tick derives distinct renew/heartbeat request ids from it.

	// Agent and ExecutionID close the runner-side observation loop
	// (dp-v2-019 d4, acceptance A10). When Agent is set, each Tick feeds
	// the Heartbeat response into Agent.Tick, which applies any newly
	// effective control revision through ControlLoop; the resulting
	// process state (terminated/checkpointed) is then reported as a
	// ProcessObservation for ExecutionID on the *next* Heartbeat, together
	// with the revision the agent applied. Leaving Agent nil (or never
	// calling Tick at all) is the negative control: without it, nothing
	// ever reports a process observation, so the verification reconciler
	// can never leave VerificationPending for this Runner.
	Agent       *ControlAgent
	ExecutionID string

	tick    int
	expired bool
	denied  bool

	pendingControlRevision domain.Revision
	pendingProcessState    string
}

// LeaseTickResult reports what one Tick call actually did.
type LeaseTickResult struct {
	Renewed         bool
	Expired         bool
	Denied          bool
	RenewResponse   application.RenewResponse
	HeartbeatResult application.LifecycleResponse
}

// Tick issues one Renew (unless the keeper has already observed the lease
// expired or denied) and one Heartbeat, using two distinct request ids
// derived from RequestBase and an internal tick counter. A stale fencing
// token is returned to the caller, not swallowed. Once a Renew fails with
// domain.ErrLeaseExpired, the keeper stops issuing further Renew calls (it
// does not try to resurrect an expired lease) but still reports heartbeats.
// Once a Renew fails with domain.ErrControlDenied (the lease's Increment is
// under a stop mode), the keeper likewise stops issuing further Renew calls,
// but unlike the expired case it still heartbeats on the very same tick:
// Heartbeat is never itself subject to Permit-based denial, and a stopped
// Runner must still be able to report a process observation through it
// (failure-model.md section 5 step 3).
func (k *LeaseKeeper) Tick(ctx context.Context, expectedLeaseVersion domain.Version, fencingToken domain.FencingToken) (LeaseTickResult, error) {
	if k == nil || k.Service == nil || k.LeaseID == "" || k.RequestBase == "" {
		return LeaseTickResult{}, errors.New("lease keeper dependencies are incomplete")
	}
	k.tick++
	var out LeaseTickResult
	if !k.expired && !k.denied {
		renewReqID := fmt.Sprintf("%s:renew-%d", k.RequestBase, k.tick)
		resp, err := k.Service.Renew(ctx, application.RenewRequest{RequestID: renewReqID, LeaseID: k.LeaseID, ExpectedLeaseVersion: expectedLeaseVersion, FencingToken: fencingToken})
		switch {
		case err == nil:
			out.Renewed = true
			out.RenewResponse = resp
		case errors.Is(err, domain.ErrLeaseExpired):
			k.expired = true
			out.Expired = true
			return out, err
		case errors.Is(err, domain.ErrControlDenied):
			k.denied = true
			out.Denied = true
			// fall through to heartbeat on this same tick.
		default:
			return out, err
		}
	} else {
		if k.expired {
			out.Expired = true
		}
		if k.denied {
			out.Denied = true
		}
	}
	heartbeatReq := application.HeartbeatRequest{RequestID: fmt.Sprintf("%s:heartbeat-%d", k.RequestBase, k.tick)}
	if k.pendingControlRevision != 0 {
		heartbeatReq.ControlRevision = k.pendingControlRevision
		if k.pendingProcessState != "" && k.ExecutionID != "" {
			heartbeatReq.Processes = []domain.ProcessObservation{{ProcessID: k.ExecutionID, State: k.pendingProcessState}}
		}
	}
	hb, err := k.Service.Heartbeat(ctx, heartbeatReq)
	if err != nil {
		return out, err
	}
	out.HeartbeatResult = hb
	if k.Agent != nil {
		state, applied, err := k.Agent.Tick(ctx, hb)
		if err != nil {
			return out, err
		}
		if applied {
			k.pendingControlRevision = k.Agent.Applied()
			k.pendingProcessState = state
		}
	}
	return out, nil
}
