// The real journey driver (V2-091).
//
// FIVE MEASURED DIFFERENCES FROM Orchestrator.RunFakeJourney, each one the
// reason a line of this file is what it is. Measured at 848d899:
//
//  1. IT DOES NOT CAPTURE. orchestrator.go:55 calls Service.Capture, so
//     RunFakeJourney MANUFACTURES the very Requirement it then works on. A
//     journey that invents its own input is the fake this task refuses. This
//     driver only claims work the Loop pass planned for a Requirement an owner
//     captured, and it learns which work that is from the offer route -- never
//     from its caller and never from a test.
//  2. IT DOES NOT FABRICATE A RUNNER IDENTITY. orchestrator.go:54 builds
//     application.Caller{Role: application.RoleRunner, Subject: o.RunnerID,
//     RunnerID: o.RunnerID} with NO session verification whatsoever. This
//     driver holds an opaque session token the server issued and the server
//     decides who it is; it constructs no application.Caller and imports
//     internal/application for nothing but one named bound.
//  3. IT DOES NOT CALL Provider.Run. orchestrator.go:123 does. Re-measured:
//     all three adapters in internal/provider/adapters.go return
//     provider.NoExec, and no Provider CLI invocation is authorised for this
//     task -- so accepting a result would be accepting one it did not obtain.
//     This driver STOPS at the provider boundary and posts NO result.
//  4. IT IS BOUNDED. At most MaxDriverClaims Increments per run, over a SINGLE
//     pass of the offered page. RunFakeJourney has no such bound because it
//     works on exactly the one Requirement it invented.
//  5. IT SHARES NO MEMORY WITH THE CONTROL PLANE. RunFakeJourney holds a
//     *application.Service pointer. This driver holds an *http.Client and a
//     base URL, and runs in a process it did not construct.
//
// Orchestrator.RunFakeJourney stays byte-unchanged and test-only:
// internal/runner/orchestrator.go and orchestrator_test.go are prohibited to
// this task, and its three call sites are all in that test file.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
)

// MaxDriverClaims is how many offered Increments ONE bounded pass may take. It
// is read from internal/application rather than declared here, so the offer the
// server builds and the bound the Runner applies cannot disagree.
const MaxDriverClaims = application.MaxDriverClaims

// LoopDriver drives one bounded pass over the work the Control Plane offered.
type LoopDriver struct {
	// Client is the injected control-plane client. Nil is refused.
	Client *ControlPlaneClient
	// Workspace is the REAL workspace this Runner creates execution directories
	// in. Nil is refused: a driver that skipped the workspace would be claiming
	// work it has nowhere to do.
	Workspace *Workspace
	// Journal is the REAL fsync-backed journal. Nil is refused.
	Journal *Journal
	// RequestNamespace makes every canonical request id this pass sends
	// deterministic and idempotent, so a repeated pass replays rather than
	// re-claims.
	RequestNamespace string
}

// DriverClaim is what one claimed Increment produced. It carries identifiers
// and a status and nothing else: no provider output, no packet, no text.
type DriverClaim struct {
	RequirementID  string `json:"requirement_id"`
	IncrementID    string `json:"increment_id"`
	ExecutionID    string `json:"execution_id"`
	LeaseID        string `json:"lease_id"`
	WorkspacePath  string `json:"workspace_path"`
	ExecutionState string `json:"execution_state"`
}

// DriverPassReport is what one bounded pass says about itself, including what
// it deliberately did NOT do.
type DriverPassReport struct {
	Offered int           `json:"offered"`
	Claimed []DriverClaim `json:"claimed"`
	// Deferred is how many offered Increments the bound left untaken, reported
	// rather than dropped.
	Deferred int `json:"deferred"`
	// Heartbeats is exactly one when the pass claimed anything, and zero when it
	// claimed nothing: a Runner that heartbeat without work would be reporting
	// liveness it was not asked for.
	Heartbeats int `json:"heartbeats"`
	// StoppedAtProviderBoundary is always true, and it is a REPORTED FACT rather
	// than a comment: this driver obtains no provider result and posts none.
	StoppedAtProviderBoundary bool `json:"stopped_at_provider_boundary"`
}

// RunOnce is the whole driver: read the offer, claim at most MaxDriverClaims of
// it, create each workspace, journal each assignment, start each Execution,
// heartbeat once, and STOP.
//
// It posts NO result. There is no code path in this file that reaches
// /v1/executions/result, and none that reaches a Provider: loopdriver_test.go
// asserts both by scanning this file's AST, and the client this driver holds
// exposes no result call at all, so the absence is structural rather than
// disciplined.
func (d *LoopDriver) RunOnce(ctx context.Context) (DriverPassReport, error) {
	report := DriverPassReport{StoppedAtProviderBoundary: true, Claimed: []DriverClaim{}}
	if d.Client == nil {
		return report, errors.New("loop driver requires an injected control-plane client")
	}
	if d.Workspace == nil {
		return report, errors.New("loop driver requires a real workspace")
	}
	if d.Journal == nil {
		return report, errors.New("loop driver requires a real journal")
	}
	if d.RequestNamespace == "" {
		return report, errors.New("loop driver requires a request namespace, so a repeated pass replays rather than re-claims")
	}
	offer, err := d.Client.OfferedWork(ctx)
	if err != nil {
		return report, fmt.Errorf("read the offered work: %w", err)
	}
	report.Offered = len(offer.Increments)
	for _, offered := range offer.Increments {
		if len(report.Claimed) >= MaxDriverClaims {
			report.Deferred++
			continue
		}
		claim, err := d.claimOne(ctx, offered)
		if err != nil {
			return report, err
		}
		report.Claimed = append(report.Claimed, claim)
	}
	if len(report.Claimed) == 0 {
		return report, nil
	}
	if _, err = d.Client.Heartbeat(ctx, d.RequestNamespace+":heartbeat"); err != nil {
		return report, fmt.Errorf("report the heartbeat: %w", err)
	}
	report.Heartbeats = 1
	return report, nil
}

// claimOne claims exactly the Increment it was OFFERED -- the increment id and
// the expected version both come from the offer -- creates its real workspace,
// appends to the real journal, and starts the Execution.
func (d *LoopDriver) claimOne(ctx context.Context, offered OfferedIncrement) (DriverClaim, error) {
	var out DriverClaim
	claimed, err := d.Client.Claim(ctx, d.RequestNamespace+":claim:"+offered.IncrementID, offered)
	if err != nil {
		return out, fmt.Errorf("claim increment %s: %w", offered.IncrementID, err)
	}
	path, err := d.Workspace.Create(claimed.ExecutionID)
	if err != nil {
		return out, fmt.Errorf("create the workspace for execution %s: %w", claimed.ExecutionID, err)
	}
	// The journal payload carries identifiers only. It carries no session
	// token, no provider output and no requirement text.
	payload, err := json.Marshal(map[string]string{
		"requirement_id": offered.RequirementID,
		"increment_id":   claimed.IncrementID,
		"execution_id":   claimed.ExecutionID,
		"lease_id":       claimed.LeaseID,
	})
	if err != nil {
		return out, err
	}
	if err = d.Journal.Append(JournalEvent{ID: claimed.ExecutionID + ":assignment", Kind: "assignment", Payload: payload}); err != nil && !errors.Is(err, ErrJournalDuplicate) {
		return out, fmt.Errorf("journal the assignment for execution %s: %w", claimed.ExecutionID, err)
	}
	// A newly created Execution is always at version one. Using the Increment's
	// version here would make the first start stale as soon as the Increment had
	// been claimed -- the same correction internal/runner/orchestrator.go's own
	// comment records.
	started, err := d.Client.StartExecution(ctx, d.RequestNamespace+":start:"+claimed.ExecutionID, claimed.ExecutionID, 1)
	if err != nil {
		return out, fmt.Errorf("start execution %s: %w", claimed.ExecutionID, err)
	}
	return DriverClaim{
		RequirementID:  offered.RequirementID,
		IncrementID:    claimed.IncrementID,
		ExecutionID:    claimed.ExecutionID,
		LeaseID:        claimed.LeaseID,
		WorkspacePath:  path,
		ExecutionState: started.Status,
	}, nil
}
