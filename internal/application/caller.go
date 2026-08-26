package application

import (
	"context"
	"errors"
	"strings"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

type Role string

const (
	RoleOwner     Role = "owner"
	RoleRunner    Role = "runner"
	RoleScheduler Role = "scheduler"
)

type Caller struct {
	Role     Role
	Subject  string
	RunnerID string
}
type callerKey struct{}

var ErrUnauthenticated = errors.New("authenticated caller is required")
var ErrForbidden = errors.New("caller is not authorized for this command")

func ContextWithCaller(ctx context.Context, caller Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, caller)
}
func CallerFromContext(ctx context.Context) (Caller, error) {
	v, ok := ctx.Value(callerKey{}).(Caller)
	if !ok {
		return Caller{}, ErrUnauthenticated
	}
	return v, nil
}

// LoopCaller is the ONLY sanctioned way to obtain a Caller carrying
// RoleScheduler, and it is the in-process analogue of internal/api's transport
// constructors: those establish an owner from an IAP assertion or a bearer
// token and a runner from a verified session, and this one establishes the Loop
// itself from a component subject its caller already authenticated.
//
// It exists because of a measurement rather than for symmetry. Before V2-086 no
// non-test site in this repository constructed a Caller with RoleScheduler at
// all, while thirteen non-test sites ACCEPTED the role, and requestedBy below is
// the sole producer of domain.ActorTypeLoop for a Requirement's, a Repository's
// and a link's RequestedBy, for a Control Intent's requester and for a
// human-input answerer. So the role was declared, accepted and unreachable, and
// every Requirement the Loop captured was recorded as owner-originated.
// Funnelling the role through one named constructor is what makes the number of
// producers greppable and assertable -- internal/application's
// requested_by_test.go asserts the repository-wide count is exactly one -- and
// gives the refusal of an empty subject one home instead of none. A bare
// composite literal is precisely how a component names itself, which is the
// defect internal/runner.Orchestrator committed with RoleOwner.
//
// An empty or whitespace-only subject is ErrUnauthenticated rather than a
// Caller with no subject: callerActor refuses an empty Subject anyway, so
// returning one would only move the failure further from its cause, and
// requestedBy would otherwise mint a domain.RequestedBy with an empty subject.
// The returned Subject is the argument as given, not a trimmed copy, so a
// caller sees back exactly the identifier it passed in.
func LoopCaller(subject string) (Caller, error) {
	if strings.TrimSpace(subject) == "" {
		return Caller{}, ErrUnauthenticated
	}
	return Caller{Role: RoleScheduler, Subject: subject}, nil
}

func callerActor(ctx context.Context, roles ...Role) (Caller, domain.ActorID, error) {
	c, err := CallerFromContext(ctx)
	if err != nil {
		return c, "", err
	}
	allowed := false
	for _, role := range roles {
		if c.Role == role {
			allowed = true
		}
	}
	if !allowed || c.Subject == "" {
		return c, "", ErrForbidden
	}
	a, err := domain.NewActorID(c.Subject)
	return c, a, err
}

// requestedBy maps an already-authenticated Caller to the domain's
// RequestedBy value. It adds no authorization semantics of its own: the
// caller's role must already have been accepted by callerActor for the
// operation in question. RoleOwner is the human owner (a local dev session
// subject, or the IAP subject in production); RoleScheduler is the Loop
// deciding on its own, identified by whatever component subject the
// internal caller set. Any other role is rejected defensively even though
// today's call sites never reach this branch with one.
func requestedBy(c Caller) (domain.RequestedBy, error) {
	switch c.Role {
	case RoleOwner:
		return domain.RequestedBy{ActorType: domain.ActorTypeOwner, Subject: c.Subject}, nil
	case RoleScheduler:
		return domain.RequestedBy{ActorType: domain.ActorTypeLoop, Subject: c.Subject}, nil
	default:
		return domain.RequestedBy{}, ErrForbidden
	}
}

func runnerCaller(ctx context.Context) (Caller, domain.ActorID, domain.RunnerID, error) {
	c, actor, err := callerActor(ctx, RoleRunner)
	if err != nil {
		return c, "", "", err
	}
	if c.RunnerID == "" {
		return c, "", "", ErrForbidden
	}
	r, err := domain.NewRunnerID(c.RunnerID)
	return c, actor, r, err
}
