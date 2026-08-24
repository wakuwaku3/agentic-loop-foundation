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

	tick    int
	expired bool
}

// LeaseTickResult reports what one Tick call actually did.
type LeaseTickResult struct {
	Renewed         bool
	Expired         bool
	RenewResponse   application.RenewResponse
	HeartbeatResult application.LifecycleResponse
}

// Tick issues one Renew (unless the keeper has already observed the lease
// expired) and one Heartbeat, using two distinct request ids derived from
// RequestBase and an internal tick counter. A stale fencing token is
// returned to the caller, not swallowed. Once a Renew fails with
// domain.ErrLeaseExpired, the keeper stops issuing further Renew calls (it
// does not try to resurrect an expired lease) but still reports heartbeats.
func (k *LeaseKeeper) Tick(ctx context.Context, expectedLeaseVersion domain.Version, fencingToken domain.FencingToken) (LeaseTickResult, error) {
	if k == nil || k.Service == nil || k.LeaseID == "" || k.RequestBase == "" {
		return LeaseTickResult{}, errors.New("lease keeper dependencies are incomplete")
	}
	k.tick++
	var out LeaseTickResult
	if !k.expired {
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
		default:
			return out, err
		}
	} else {
		out.Expired = true
	}
	heartbeatReqID := fmt.Sprintf("%s:heartbeat-%d", k.RequestBase, k.tick)
	hb, err := k.Service.Heartbeat(ctx, application.HeartbeatRequest{RequestID: heartbeatReqID})
	if err != nil {
		return out, err
	}
	out.HeartbeatResult = hb
	return out, nil
}
