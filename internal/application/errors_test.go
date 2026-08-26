package application_test

// V2-083 A9: the application layer's half of the caller-fault classification,
// asserted in BOTH directions.
//
// The positive half says that every validation refusal a route can reach
// carries the ErrInvalidRequest identity, which is what keeps those responses
// at 400 invalid_request now that internal/api's domainError answers 500 for
// anything it cannot classify.
//
// The negative half is not optional. Without it the marker drifts toward
// being applied everywhere -- at a shared helper, in Service.mutate, around a
// transaction -- and a blanket wrap turns every 500 back into a 400, which is
// the original defect with a new spelling. So a fault the request did not
// cause is asserted NOT to satisfy errors.Is(err, ErrInvalidRequest): a clock
// that returns the zero instant, and a store decorator's storage error.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// errorClassTransactor is the negative half's fault source: a decorator over
// the Transactor port that answers with storage-shaped text instead of
// running the callback. Nothing about it is a caller mistake.
type errorClassTransactor struct {
	inner application.Transactor
	fault error
	armed bool
}

func (t *errorClassTransactor) Transact(ctx context.Context, fn func(application.UnitOfWork) error) error {
	if !t.armed {
		return t.inner.Transact(ctx, fn)
	}
	return t.fault
}

// errorClassStoreFaultText is the same storage-shaped text internal/api's A7
// test injects: a doubled Firestore parent name with a byte offset.
const errorClassStoreFaultText = "rpc error: code = InvalidArgument desc = Invalid StartAfter value: " +
	"projects/agentic-loop-local/databases/(default)/documents/installations/install-7f3a/requirements/" +
	"projects/agentic-loop-local/databases/(default)/documents/installations/install-7f3a/requirements/" +
	"cmVxdWlyZW1lbnQtYg at byte offset 118"

// errorClassStepClock is stepped a whole UTC day between read transactions:
// a read reserves the conservative read boundary against the daily quota
// budget and the memory adapter never trues that reservation up, so a fixed
// clock allows only four read transactions per UTC day per store. It is an
// injected clock advanced explicitly, never a sleep, a timer or a goroutine.
type errorClassStepClock struct{ at time.Time }

func (c *errorClassStepClock) Now() time.Time { return c.at }

func (c *errorClassStepClock) nextDay() { c.at = c.at.Add(25 * time.Hour) }

func TestEveryCallerFaultCarriesTheInvalidRequestIdentityAndNoOtherFaultDoes(t *testing.T) {
	// --- the positive half ------------------------------------------------
	st := memory.New()
	clk := &errorClassStepClock{at: time.Unix(1700000000, 0).UTC()}
	svc, err := application.NewServiceWithConfig(st, clk, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())

	callerFaults := []struct {
		name        string
		wantMessage string
		run         func() error
	}{
		{"capture-with-no-request-id", "request_id is required", func() error {
			_, e := svc.Capture(ctx, application.CaptureRequest{Text: "x"})
			return e
		}},
		{"control-with-no-request-id", "request_id is required", func() error {
			_, e := svc.Control(ctx, application.ControlRequest{Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: domain.ControlAllow})
			return e
		}},
		{"register-repository-with-no-request-id", "request_id is required", func() error {
			_, e := svc.RegisterRepository(ctx, application.RegisterRepositoryRequest{SourceURL: "https://github.com/o/n"})
			return e
		}},
		{"heartbeat-with-no-request-id", "request_id is required", func() error {
			_, e := svc.Heartbeat(runner(context.Background(), "runner-1"), application.HeartbeatRequest{})
			return e
		}},
		{"start-framing-with-no-request-id", "request_id is required", func() error {
			_, e := svc.StartFraming(ctx, application.StartFramingRequest{RequirementID: "requirement-x"})
			return e
		}},
		{"control-with-a-mode-that-is-not-one-of-the-seven", "invalid control mode or scope", func() error {
			_, e := svc.Control(ctx, application.ControlRequest{RequestID: "bad-mode", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: domain.ControlMode("not-a-mode")})
			return e
		}},
		{"page-size-below-one", "page_size must be between 1 and 100", func() error {
			_, e := svc.ListRequirementsPage(ctx, "", -1)
			return e
		}},
		{"page-size-above-the-maximum", "page_size must be between 1 and 100", func() error {
			_, e := svc.ListRequirementsPage(ctx, "", application.MaxPageSize+1)
			return e
		}},
		{"controls-page-size-above-the-maximum", "page_size must be at most 100", func() error {
			_, e := svc.ListControls(ctx, application.MaxPageSize+1)
			return e
		}},
		{"export-limit-below-one", "export limit must be between 1 and 100", func() error {
			_, e := svc.Export(ctx, 0)
			return e
		}},
		{"export-limit-above-the-maximum", "export limit must be between 1 and 100", func() error {
			_, e := svc.Export(ctx, application.MaxPageSize+1)
			return e
		}},
	}
	for _, c := range callerFaults {
		c := c
		t.Run("caller-fault/"+c.name, func(t *testing.T) {
			clk.nextDay()
			e := c.run()
			if e == nil {
				t.Fatal("the refusal did not happen at all")
			}
			if !errors.Is(e, application.ErrInvalidRequest) {
				t.Fatalf("errors.Is(err, ErrInvalidRequest) = false for %v", e)
			}
			// The marker must not change what a caller reads. This is the
			// property that lets internal/api's pin table compare whole
			// response bodies byte for byte before and after the change.
			if e.Error() != c.wantMessage {
				t.Fatalf("message = %q, want the unchanged %q", e.Error(), c.wantMessage)
			}
			if strings.Contains(e.Error(), application.ErrInvalidRequest.Error()) {
				t.Fatalf("the sentinel's own text leaked into the message: %q", e.Error())
			}
		})
	}

	// --- the caller faults that ALREADY carried a sentinel ----------------
	//
	// These are not marked with ErrInvalidRequest: they carry their own
	// exported sentinel, and internal/api's isCallerFault names each of them
	// explicitly. Asserting the identity here is what makes that list a
	// measured claim rather than a guess -- if one of these ever stopped
	// carrying its sentinel, its route would answer 500 instead of 400.
	sentinelledCallerFaults := []struct {
		name     string
		sentinel error
		run      func() error
	}{
		{"a-cursor-the-route-never-issued", application.ErrInvalidCursor, func() error {
			_, e := svc.ListRequirementsPage(ctx, "fabricated.cursor*", 1)
			return e
		}},
		{"a-provider-name-outside-the-closed-set", application.ErrProviderUnknown, func() error {
			_, e := svc.Start(runner(context.Background(), "runner-1"), application.StartRequest{RequestID: "bad-provider", ExecutionID: "execution-x", Provider: application.ProviderName("not-a-provider")})
			return e
		}},
		{"a-provider-observation-naming-an-unknown-provider", application.ErrProviderUnknown, func() error {
			_, e := svc.AcceptResult(runner(context.Background(), "runner-1"), application.AcceptResultRequest{RequestID: "bad-observation", ExecutionID: "execution-x", LeaseID: "lease-x",
				ProviderObservation: &application.ProviderObservationInput{Name: application.ProviderName("not-a-provider")}})
			return e
		}},
		{"an-incomplete-runner-version-report", application.ErrRunnerVersionReportInvalid, func() error {
			_, e := svc.Heartbeat(runner(context.Background(), "runner-1"), application.HeartbeatRequest{RequestID: "bad-report", RunnerVersion: &application.RunnerVersionInput{}})
			return e
		}},
		{"an-allocation-limit-outside-the-closed-range", application.ErrAllocationLimitOutOfRange, func() error {
			_, e := svc.Control(ctx, application.ControlRequest{RequestID: "bad-limit", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: domain.ControlAllow,
				AllocationLimit: &application.AllocationLimitInput{InstallationConcurrentExecutions: -1}})
			return e
		}},
		{"a-source-locator-that-does-not-parse", domain.ErrInvalidSourceLocator, func() error {
			_, e := svc.RegisterRepository(ctx, application.RegisterRepositoryRequest{RequestID: "bad-locator", SourceURL: "not a locator"})
			return e
		}},
	}
	for _, c := range sentinelledCallerFaults {
		c := c
		t.Run("sentinelled-caller-fault/"+c.name, func(t *testing.T) {
			clk.nextDay()
			e := c.run()
			if e == nil {
				t.Fatal("the refusal did not happen at all")
			}
			if !errors.Is(e, c.sentinel) {
				t.Fatalf("errors.Is(err, %v) = false for %v", c.sentinel, e)
			}
		})
	}

	// --- the negative half ------------------------------------------------
	zeroClockStore := memory.New()
	zeroClockService, err := application.NewServiceWithConfig(zeroClockStore, zeroClock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	faultStore := memory.New()
	fault := &errorClassTransactor{inner: faultStore, fault: errors.New(errorClassStoreFaultText)}
	faultClock := &errorClassStepClock{at: time.Unix(1700000000, 0).UTC()}
	faultService, err := application.NewServiceWithConfig(fault, faultClock, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	fault.armed = true

	notCallerFaults := []struct {
		name string
		run  func() error
	}{
		{"a-clock-that-returned-the-zero-instant-on-capture", func() error {
			_, e := zeroClockService.Capture(ctx, application.CaptureRequest{RequestID: "zero-clock-capture", Text: "x"})
			return e
		}},
		{"a-clock-that-returned-the-zero-instant-on-the-queue-summary", func() error {
			_, e := zeroClockService.QueueSummary(ctx)
			return e
		}},
		{"a-clock-that-returned-the-zero-instant-on-the-provider-registry", func() error {
			_, e := zeroClockService.Providers(ctx)
			return e
		}},
		{"a-clock-that-returned-the-zero-instant-on-the-runner-report", func() error {
			_, e := zeroClockService.Runners(ctx)
			return e
		}},
		{"a-store-decorators-storage-error-on-a-read", func() error {
			_, e := faultService.ListRequirementsPage(ctx, "", 1)
			return e
		}},
		{"a-store-decorators-storage-error-on-a-mutation", func() error {
			_, e := faultService.Capture(ctx, application.CaptureRequest{RequestID: "store-fault-capture", Text: "x"})
			return e
		}},
		{"a-store-decorators-storage-error-on-the-repository-list", func() error {
			_, e := faultService.ListRepositories(ctx)
			return e
		}},
	}
	for _, c := range notCallerFaults {
		c := c
		t.Run("not-a-caller-fault/"+c.name, func(t *testing.T) {
			faultClock.nextDay()
			e := c.run()
			if e == nil {
				t.Fatal("the fault was swallowed and the operation reported success")
			}
			if errors.Is(e, application.ErrInvalidRequest) {
				t.Fatalf("a fault the request did not cause claims the caller-fault identity: %v", e)
			}
		})
	}
}
