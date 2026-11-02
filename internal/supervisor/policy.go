// Package supervisor holds the restart policy the workflow engine applies
// to agents that exit retryably, after Erlang/OTP's transient children:
// restart from the exit's checkpoint, with backoff, at most Max times
// within Within, and past that the failure goes to the parent. The engine
// (agentflow) does the restarting through the runtime's exclusive Recover;
// this package only says when and how long to wait.
package supervisor

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrInvalidPolicy = errors.New("invalid restart policy")

// Defaults for a policy's fields left empty. Intensity is always bounded:
// a policy cannot restart without limit.
const (
	DefaultMax     = 3
	DefaultWithin  = 10 * time.Minute
	DefaultBackoff = "1s..30s"
)

// RestartPolicy is an agent's restart field in a workflow spec, for
// example {"max": 3, "within": "10m", "backoff": "1s..2m"}.
type RestartPolicy struct {
	// Max is how many restarts are allowed within Within; one more is the
	// intensity being exceeded. Zero means DefaultMax.
	Max int `json:"max,omitempty"`
	// Within is the window restarts are counted in, a Go duration. Empty
	// means DefaultWithin.
	Within string `json:"within,omitempty"`
	// Backoff is the wait before a restart: "base..max" doubles from base
	// on each restart in the window up to max; a single duration is a
	// fixed wait. Empty means DefaultBackoff.
	Backoff string `json:"backoff,omitempty"`
}

// Policy is a RestartPolicy with its defaults applied and parsed.
type Policy struct {
	Max         int
	Within      time.Duration
	BackoffBase time.Duration
	BackoffMax  time.Duration
}

// Effective applies the defaults and parses the durations.
func (p RestartPolicy) Effective() (Policy, error) {
	policy := Policy{Max: p.Max, Within: DefaultWithin}
	if policy.Max < 0 {
		return Policy{}, fmt.Errorf("%w: max %d is negative", ErrInvalidPolicy, p.Max)
	}
	if policy.Max == 0 {
		policy.Max = DefaultMax
	}
	if within := strings.TrimSpace(p.Within); within != "" {
		parsed, err := time.ParseDuration(within)
		if err != nil || parsed <= 0 {
			return Policy{}, fmt.Errorf("%w: within %q is not a positive duration", ErrInvalidPolicy, p.Within)
		}
		policy.Within = parsed
	}
	backoff := strings.TrimSpace(p.Backoff)
	if backoff == "" {
		backoff = DefaultBackoff
	}
	base, most, ranged := strings.Cut(backoff, "..")
	if !ranged {
		most = base
	}
	var err error
	if policy.BackoffBase, err = time.ParseDuration(strings.TrimSpace(base)); err != nil || policy.BackoffBase < 0 {
		return Policy{}, fmt.Errorf("%w: backoff %q", ErrInvalidPolicy, p.Backoff)
	}
	if policy.BackoffMax, err = time.ParseDuration(strings.TrimSpace(most)); err != nil || policy.BackoffMax < policy.BackoffBase {
		return Policy{}, fmt.Errorf("%w: backoff %q", ErrInvalidPolicy, p.Backoff)
	}
	return policy, nil
}

// Backoff is the wait before the nth restart in the window (from 1): the
// base, doubled for each earlier one, at most the maximum.
func (p Policy) Backoff(n int) time.Duration {
	wait := p.BackoffBase
	for i := 1; i < n && wait < p.BackoffMax; i++ {
		wait *= 2
	}
	return min(wait, p.BackoffMax)
}

// Recent returns how many of restarts, the times earlier restarts began,
// fall within the window ending at now.
func (p Policy) Recent(restarts []time.Time, now time.Time) int {
	n := 0
	for _, at := range restarts {
		if now.Sub(at) < p.Within {
			n++
		}
	}
	return n
}

// Exceeded reports whether one more restart at now would exceed the
// intensity: Max restarts already fall within the window.
func (p Policy) Exceeded(restarts []time.Time, now time.Time) bool {
	return p.Recent(restarts, now) >= p.Max
}
