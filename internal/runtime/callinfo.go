package runtime

import "context"

// CallInfo says whom a model call is for, so a backend that records its
// calls can attribute them without knowing about processes.
type CallInfo struct {
	PID     string
	AgentID string
	// Repair marks a retry that asks the model to fix a malformed reply.
	Repair bool
	// MaxTokens, when positive, caps the completion: what is left of the
	// process's token budget.
	MaxTokens int
	// TokensLeft, when positive, is what is left of the process's token
	// budget for the step's calls, prompt and reply. A driver caps each
	// call's reply at it, less what Meter has counted this step and its
	// estimate of the prompt it is about to send, and refuses the call with
	// ErrNoReplyTokens when that leaves nothing.
	TokensLeft int
	// Meter, when set, receives the tokens each call (repairs included)
	// used, as the server reports them.
	Meter *TokenMeter
	// Step is the process's step the call belongs to (Usage.Steps), and
	// Elapsed the seconds its budget chain's top account had used when the
	// step began: the reading its budget notes came from, which a replay
	// sets its clock to.
	Step    int
	Elapsed float64
	// Run is the kernel event that started the run (run.started), if any.
	Run string
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

type runIDKey struct{}

// WithRunID attaches the run.started event's ID to ctx, for the calls the
// run makes to name.
func WithRunID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, runIDKey{}, id)
}

// RunIDFrom returns the run.started event's ID attached to ctx, if any.
func RunIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(runIDKey{}).(string)
	return id
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
