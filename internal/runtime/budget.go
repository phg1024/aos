package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"aos/internal/capability"
	"aos/internal/framing"
)

// ErrTimeBudgetExceeded is the cause of a run stopped by a strict time
// budget.
var ErrTimeBudgetExceeded = errors.New("time budget exceeded")

// ErrTerminated is the cause of a run stopped by a termination signal
// (aos run on SIGTERM or SIGINT). Like a spent budget, it ends the process
// retryably, with a final checkpoint.
var ErrTerminated = errors.New("terminated by signal")

// budgetWarningFraction is the share of a budget left at which the agent is
// told, once, to commit its best answer now.
const budgetWarningFraction = 0.1

// Usage is what a process has consumed, kept in its checkpoints so a
// resumed or recovered process goes on against the same budget. Tokens and
// Seconds include what its delegates consumed; Steps are its own.
type Usage struct {
	Steps   int     `json:"steps,omitempty"`
	Tokens  int     `json:"tokens,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
}

// Extension is extra budget an operator grants explicitly, for instance
// when recovering a process that ran out. It accumulates in the session.
type Extension struct {
	Steps   int `json:"steps,omitempty"`
	Tokens  int `json:"tokens,omitempty"`
	Seconds int `json:"seconds,omitempty"`
}

func (e Extension) add(more Extension) Extension {
	return Extension{Steps: e.Steps + more.Steps, Tokens: e.Tokens + more.Tokens, Seconds: e.Seconds + more.Seconds}
}

// Account meters one budget: a process's, or a whole run's. Accounts chain
// to the account of whoever delegated, like cgroups: tokens charged to a
// child are charged to every ancestor, and a limit anywhere up the chain
// binds the child too.
type Account struct {
	Owner  string
	Limit  capability.Budget
	parent *Account

	mu      sync.Mutex
	tokens  int
	seconds float64
	since   time.Time
	now     func() time.Time
}

// NewAccount starts an account that has already used used.
func NewAccount(owner string, limit capability.Budget, used Usage, parent *Account) *Account {
	return &Account{Owner: owner, Limit: limit, parent: parent, tokens: used.Tokens, seconds: used.Seconds, now: time.Now}
}

// SetClock makes the account read time from now instead of the wall
// clock, as a replay does.
func (a *Account) SetClock(now func() time.Time) {
	if now != nil {
		a.now = now
	}
}

// Start and Stop bracket the time the account's owner runs.
func (a *Account) Start() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.since.IsZero() {
		a.since = a.now()
	}
}

func (a *Account) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.since.IsZero() {
		a.seconds += a.now().Sub(a.since).Seconds()
		a.since = time.Time{}
	}
}

// AddTokens charges n tokens here and to every ancestor.
func (a *Account) AddTokens(n int) {
	for account := a; account != nil; account = account.parent {
		account.mu.Lock()
		account.tokens += n
		account.mu.Unlock()
	}
}

// Used is the tokens and seconds consumed so far.
func (a *Account) Used() (int, float64) {
	return a.UsedAt(a.now())
}

// UsedAt is the tokens consumed so far and the seconds consumed by time t.
func (a *Account) UsedAt(t time.Time) (int, float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	seconds := a.seconds
	if !a.since.IsZero() {
		seconds += t.Sub(a.since).Seconds()
	}
	return a.tokens, seconds
}

// top is the account at the head of a's chain: the run's, in a workflow.
func (a *Account) top() *Account {
	for a.parent != nil {
		a = a.parent
	}
	return a
}

// Limited is the tightest remaining amount over the chain: how much is
// left, of whose budget and of what limit. ok is false when no account in
// the chain limits it.
type Limited struct {
	Left  float64
	Limit int
	Owner string
	ok    bool
}

func (a *Account) tokensLeft() Limited {
	var tightest Limited
	for account := a; account != nil; account = account.parent {
		if account.Limit.Tokens <= 0 {
			continue
		}
		used, _ := account.Used()
		left := float64(account.Limit.Tokens - used)
		if !tightest.ok || left < tightest.Left {
			tightest = Limited{Left: left, Limit: account.Limit.Tokens, Owner: account.Owner, ok: true}
		}
	}
	return tightest
}

// timeLeft is the tightest time budget over the chain; strict selects only
// strict budgets.
func (a *Account) timeLeft(strictOnly bool) Limited {
	return a.timeLeftAt(strictOnly, a.now())
}

// timeLeftAt is timeLeft at time t.
func (a *Account) timeLeftAt(strictOnly bool, t time.Time) Limited {
	var tightest Limited
	for account := a; account != nil; account = account.parent {
		if account.Limit.Seconds <= 0 || (strictOnly && !account.Limit.StrictTime) {
			continue
		}
		_, used := account.UsedAt(t)
		left := float64(account.Limit.Seconds) - used
		if !tightest.ok || left < tightest.Left {
			tightest = Limited{Left: left, Limit: account.Limit.Seconds, Owner: account.Owner, ok: true}
		}
	}
	return tightest
}

type accountKey struct{}

// WithAccount makes account the parent of accounts started under ctx, as a
// delegator's is for its delegates.
func WithAccount(ctx context.Context, account *Account) context.Context {
	return context.WithValue(ctx, accountKey{}, account)
}

// AccountFrom returns the account attached to ctx, if any.
func AccountFrom(ctx context.Context) *Account {
	account, _ := ctx.Value(accountKey{}).(*Account)
	return account
}

// TokenMeter collects the tokens a model call (and its repairs) used, as
// the backend reports them.
type TokenMeter struct{ total atomic.Int64 }

func (m *TokenMeter) Add(tokens int) { m.total.Add(int64(tokens)) }
func (m *TokenMeter) Total() int     { return int(m.total.Load()) }

// budgetNotes is what the agent is told about its budgets this step: the
// time and tokens left, the steps left once few are, and once per budget
// kind, when less than a tenth is left, to commit its best answer now.
//
// Time and token notes come every step a budget is set. Every run has a
// step limit, so a steps note every step would add a message to every run,
// shifting when compaction cuts and changing every prompt; it comes only in
// the last tenth of the steps instead, the warning with its first. Steps
// count from the start of the turn (StepBase) and include this one. The
// last step is always in that stretch, so a limit under ten steps still
// gets the warning. Time is read once, at now, so the notes depend only on
// that reading, which the step's model calls record.
func budgetNotes(account *Account, session *Session, maxSteps int, now time.Time) []Message {
	var notes []Message
	if maxSteps > 0 {
		used := session.Usage.Steps - session.StepBase - 1
		left := maxSteps - used
		if float64(left) <= max(float64(maxSteps)*budgetWarningFraction, 1) {
			notes = append(notes, runtimeNote("tool", framing.BudgetSteps, fmt.Sprintf("budget steps=%d used=%d left=%d (strict: the run is stopped after the last step)", maxSteps, used, left)))
			if !session.warned("steps") {
				session.BudgetWarned = append(session.BudgetWarned, "steps")
				notes = append(notes, runtimeNote("tool", framing.BudgetWarning, fmt.Sprintf("budget warning: %d of %d steps are left, this one included. Commit your best answer now: write or submit what you have, then exit.", left, maxSteps)))
			}
		}
	}
	time := account.timeLeftAt(false, now)
	if time.ok {
		left := max(time.Left, 0)
		note := fmt.Sprintf("budget time=%ds used=%ds left=%ds", time.Limit, int(float64(time.Limit)-left), int(left))
		if account.timeLeftAt(true, now).ok {
			note += " (strict: the run is stopped when it runs out)"
		}
		notes = append(notes, runtimeNote("tool", framing.BudgetTime, note))
		if left <= float64(time.Limit)*budgetWarningFraction && !session.warned("time") {
			session.BudgetWarned = append(session.BudgetWarned, "time")
			notes = append(notes, runtimeNote("tool", framing.BudgetWarning, "budget warning: less than 10% of the time is left. Commit your best answer now: write or submit what you have, then exit."))
		}
	}
	tokens := account.tokensLeft()
	if tokens.ok {
		left := max(tokens.Left, 0)
		notes = append(notes, runtimeNote("tool", framing.BudgetTokens, fmt.Sprintf("budget tokens=%d used=%d left=%d", tokens.Limit, tokens.Limit-int(left), int(left))))
		if left <= float64(tokens.Limit)*budgetWarningFraction && !session.warned("tokens") {
			session.BudgetWarned = append(session.BudgetWarned, "tokens")
			notes = append(notes, runtimeNote("tool", framing.BudgetWarning, "budget warning: less than 10% of the tokens are left. Commit your best answer now: write or submit what you have, then exit."))
		}
	}
	return notes
}

func (s Session) warned(kind string) bool {
	for _, done := range s.BudgetWarned {
		if done == kind {
			return true
		}
	}
	return false
}

func timeBudgetError(limit Limited) error {
	return fmt.Errorf("%w: %ds strict budget of %s", ErrTimeBudgetExceeded, limit.Limit, limit.Owner)
}

// SpentBudget says why a process continued from checkpointID could not
// take a step: its step limit (maxSteps, as AgentRuntime.MaxSteps), its
// token budget or a strict time budget, its own or one up the account
// chain in ctx, is already used up. It is empty when a step could run.
// Advisory time does not stop a run, so it never counts as spent.
func (r *AgentRuntime) SpentBudget(ctx context.Context, pid, checkpointID string, maxSteps int) (string, error) {
	process, err := r.Kernel.Get(pid)
	if err != nil {
		return "", err
	}
	session, err := r.ownedCheckpoint(pid, checkpointID)
	if err != nil {
		return "", err
	}
	if maxSteps <= 0 {
		maxSteps = 32
	}
	if steps := maxSteps + session.Extended.Steps; session.Usage.Steps-session.StepBase >= steps {
		return fmt.Sprintf("step limit reached: %d of %d steps used", session.Usage.Steps-session.StepBase, steps), nil
	}
	limit := process.CapabilitySet.Budget
	if limit.Tokens > 0 {
		limit.Tokens += session.Extended.Tokens
	}
	if limit.Seconds > 0 {
		limit.Seconds += session.Extended.Seconds
	}
	account := NewAccount(pid, limit, session.Usage, AccountFrom(ctx))
	account.SetClock(r.Clock)
	if tokens := account.tokensLeft(); tokens.ok && tokens.Left <= 0 {
		return fmt.Sprintf("token budget of %s spent: %d of %d tokens used", tokens.Owner, tokens.Limit-int(tokens.Left), tokens.Limit), nil
	}
	if strict := account.timeLeft(true); strict.ok && strict.Left <= 0 {
		return fmt.Sprintf("strict time budget of %s spent: %ds", strict.Owner, strict.Limit), nil
	}
	return "", nil
}
