package runtime

import "context"

// CallInfo says whom a model call is for, so a backend that records its
// calls can attribute them without knowing about processes.
type CallInfo struct {
	PID     string
	AgentID string
	// Repair marks a retry that asks the model to fix a malformed reply.
	Repair bool
}

type callInfoKey struct{}

// WithCallInfo attaches info to the context of a model call.
func WithCallInfo(ctx context.Context, info CallInfo) context.Context {
	return context.WithValue(ctx, callInfoKey{}, info)
}

// CallInfoFrom returns the call info attached to ctx, if any.
func CallInfoFrom(ctx context.Context) (CallInfo, bool) {
	info, ok := ctx.Value(callInfoKey{}).(CallInfo)
	return info, ok
}

// AsRepair marks ctx's call info, if any, as a repair retry.
func AsRepair(ctx context.Context) context.Context {
	info, ok := CallInfoFrom(ctx)
	if !ok {
		return ctx
	}
	info.Repair = true
	return WithCallInfo(ctx, info)
}
