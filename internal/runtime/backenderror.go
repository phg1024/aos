package runtime

import (
	"errors"
	"time"
)

// BackendError is a failed model call whose driver says whether the same
// request may succeed if sent again. The step retries a temporary one, as
// BackendRetryPolicy says, and ends at once on any other: a 404 or a bad
// key answers the same way twelve times. An error that is not a
// BackendError, such as a dropped connection or a timeout, is retried, as
// every error was before drivers told them apart; so is a recorded error
// from before then, which a replay rebuilds as a plain error.
type BackendError interface {
	error
	Temporary() bool
}

// BackendRetryAfter is a BackendError that carries the wait the server
// asked for (retry-after). The retry waits that long instead of the
// policy's delay, capped at the policy's Max.
type BackendRetryAfter interface {
	RetryWait() time.Duration
}

// ErrNoReplyTokens is a call a driver refused before sending: what is left
// of the token budget does not cover the prompt it was about to send, so
// no reply could fit. The step ends as a spent token budget does.
var ErrNoReplyTokens = errors.New("token budget exceeded: no tokens left for a reply")

// retryBackendError reports whether a failed call may be retried, and the
// wait its server asked for, if any.
func retryBackendError(err error) (bool, time.Duration) {
	var failure BackendError
	if !errors.As(err, &failure) {
		return true, 0
	}
	if !failure.Temporary() {
		return false, 0
	}
	var after BackendRetryAfter
	if errors.As(err, &after) {
		return true, after.RetryWait()
	}
	return true, 0
}
