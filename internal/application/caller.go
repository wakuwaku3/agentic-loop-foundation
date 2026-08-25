package application

import (
	"context"
	"errors"

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
