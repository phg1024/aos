package runtime

import (
	"fmt"
	"time"
)

// clockWarningFraction is the share of the budget left at which clock notes
// start warning the agent to secure its answer.
const clockWarningFraction = 0.2

// Clock gives an agent a wall-clock budget. When set on AgentRuntime, every
// step appends a note with the time used and left, so the agent can pace
// itself against a deadline it cannot otherwise see. Notes go at the end of
// the history, which keeps the cached prompt prefix intact.
type Clock struct {
	Deadline time.Time
	Budget   time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
}

// NewClock starts a budget of the given length now.
func NewClock(budget time.Duration) *Clock {
	return &Clock{Deadline: time.Now().Add(budget), Budget: budget}
}

// Note reports elapsed and remaining time, with a warning near the deadline.
func (c *Clock) Note() string {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	remaining := c.Deadline.Sub(now())
	if remaining < 0 {
		remaining = 0
	}
	elapsed := c.Budget - remaining
	if elapsed < 0 {
		elapsed = 0
	}
	note := fmt.Sprintf("clock time_budget=%s elapsed=%s remaining=%s",
		c.Budget.Round(time.Second), elapsed.Round(time.Second), remaining.Round(time.Second))
	if c.Budget > 0 && remaining <= time.Duration(float64(c.Budget)*clockWarningFraction) {
		note += " warning=less_than_20_percent_left_save_a_complete_answer_now"
	}
	return note
}
