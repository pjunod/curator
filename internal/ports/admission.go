package ports

import (
	"context"
	"errors"
)

// ErrMaintenance means new work is paused for a deployment drain.
var ErrMaintenance = errors.New("deployment maintenance: new work is paused")

// WorkAdmission registers actual work before side effects and retains ownership
// through its completion. Enter waits for resume; TryEnter rejects while paused.
// Already admitted parents can register children before releasing their token.
type WorkAdmission interface {
	Enter(context.Context, string) (context.Context, func(), error)
	TryEnter(context.Context, string) (context.Context, func(), error)
}

func EnterWork(ctx context.Context, gate WorkAdmission, kind string) (context.Context, func(), error) {
	if gate == nil {
		return ctx, func() {}, ctx.Err()
	}
	return gate.Enter(ctx, kind)
}

func TryWork(ctx context.Context, gate WorkAdmission, kind string) (context.Context, func(), error) {
	if gate == nil {
		return ctx, func() {}, ctx.Err()
	}
	return gate.TryEnter(ctx, kind)
}
