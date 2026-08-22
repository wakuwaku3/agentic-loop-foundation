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
