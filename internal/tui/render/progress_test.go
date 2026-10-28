package render

import (
	"strings"
	"testing"
	"time"

	"aos/internal/agentflow"
	"aos/internal/capability"
)

func testProgress(clock *time.Time) *WorkflowProgress {
	progress := NewWorkflowProgress(Palette{}, agentflow.WorkflowSpec{
		Name: "metal",
		Agents: []agentflow.AgentSpec{
			{ID: "coordinator", MaxSteps: 400},
			{ID: "writer", MaxSteps: 200},
		},
	})
	progress.now = func() time.Time { return *clock }
	return progress
}

func event(clock time.Time, agentID, pid, kind, detail string) agentflow.TraceEvent {
	return agentflow.TraceEvent{Time: clock, AgentID: agentID, PID: pid, Kind: kind, Detail: detail}
}

func TestWorkflowProgressTracksDelegationStack(t *testing.T) {
	clock := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	progress := testProgress(&clock)

	if progress.Active() {
		t.Fatal("new progress should be idle")
	}
	progress.Apply(event(clock, "coordinator", "pid-2", "start", "build it"))
	progress.Apply(event(clock, "coordinator", "pid-2", "tool_call", "read_file path=/x"))
	progress.Apply(event(clock, "coordinator", "pid-2", "delegate", "to=writer goal=write"))

	clock = clock.Add(time.Minute)
	start := progress.Apply(event(clock, "writer", "pid-3", "start", "write\nthe   code"))
	if !strings.HasPrefix(start, "  ┌ writer pid-3") || !strings.Contains(start, "goal: write the code") {
		t.Fatalf("child start should be indented one level with a flattened goal, got %q", start)
	}
	call := progress.Apply(event(clock, "writer", "pid-3", "tool_call", "run command=clang++"))
	if call != "  │ writer › run command=clang++" {
		t.Fatalf("unexpected tool call line %q", call)
	}
	progress.Apply(event(clock, "writer", "pid-3", "tool_result", "run exit=0"))
	progress.Apply(event(clock, "writer", "pid-3", "tool_call", "edit_file path=/y"))

	clock = clock.Add(14*time.Minute + 2*time.Second)
	status := progress.StatusLine()
	for _, want := range []string{"workflow metal", "coordinator 2/400 › writer 2/200", "14m02s", "tool edit_file", "finished=0"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status %q missing %q", status, want)
		}
	}

	exit := progress.Apply(event(clock, "writer", "pid-3", "exit", "code=0 reason=done"))
	if !strings.HasPrefix(exit, "  └ writer exited") {
		t.Fatalf("exit should render at the child's depth, got %q", exit)
	}
	if line := progress.Apply(event(clock, "coordinator", "pid-2", "delegate_result", "from=writer")); line != "" {
		t.Fatalf("delegate_result should only update status, got %q", line)
	}
	status = progress.StatusLine()
	if strings.Contains(status, "writer") || !strings.Contains(status, "coordinator 2/400 | 15m02s | thinking | finished=1") {
		t.Fatalf("writer should be popped and coordinator thinking, got %q", status)
	}

	progress.Observe(event(clock, "coordinator", "pid-2", "exit", ""))
	if progress.Active() || !strings.Contains(progress.StatusLine(), "idle | finished=1") {
		t.Fatalf("expected idle after root exit, got %q", progress.StatusLine())
	}
}

func TestWorkflowProgressIgnoresDuplicateStartAndUnknownExit(t *testing.T) {
	clock := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	progress := testProgress(&clock)
	progress.Observe(event(clock, "coordinator", "pid-2", "start", ""))
	progress.Observe(event(clock, "coordinator", "pid-2", "start", ""))
	progress.Observe(event(clock, "writer", "pid-9", "exit", ""))
	if got := progress.Depth("pid-2"); got != 0 {
		t.Fatalf("depth of root = %d, want 0", got)
	}
	if !strings.Contains(progress.StatusLine(), "finished=0") {
		t.Fatalf("unknown exit should not count, got %q", progress.StatusLine())
	}
}

func TestFormatElapsedAndClipLine(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{{0, "0m00s"}, {62 * time.Second, "1m02s"}, {3*time.Hour + 4*time.Minute + 5*time.Second, "3h04m05s"}, {-time.Second, "0m00s"}} {
		if got := formatElapsed(tc.in); got != tc.want {
			t.Fatalf("formatElapsed(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := clipLine("渲染 bunny\n with   metal", 8); got != "渲染 bunny…" {
		t.Fatalf("clipLine should cut on runes, got %q", got)
	}
}

func TestWorkflowProgressRendersCancelledChild(t *testing.T) {
	clock := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	progress := testProgress(&clock)
	progress.Apply(event(clock, "coordinator", "pid-2", "start", ""))
	progress.Apply(event(clock, "writer", "pid-3", "start", ""))
	line := progress.Apply(event(clock, "writer", "pid-3", "cancelled", "run cancelled: context canceled"))
	if line != "  └ writer cancelled" {
		t.Fatalf("unexpected cancelled line %q", line)
	}
	if status := progress.StatusLine(); strings.Contains(status, "writer") || !strings.Contains(status, "finished=1") {
		t.Fatalf("cancelled child should be popped and counted, got %q", status)
	}
}

func TestIsLoneEsc(t *testing.T) {
	for _, tc := range []struct {
		input []byte
		want  bool
	}{
		{[]byte{0x1b}, true},
		{[]byte("\x1b[A"), false}, // up arrow
		{[]byte("\x1bOP"), false}, // F1
		{[]byte("a"), false},
		{nil, false},
	} {
		if got := isLoneEsc(tc.input); got != tc.want {
			t.Fatalf("isLoneEsc(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestWorkflowProgressMarksFailedChildExit(t *testing.T) {
	clock := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	progress := testProgress(&clock)
	progress.Apply(event(clock, "coordinator", "pid-2", "start", ""))
	progress.Apply(event(clock, "writer", "pid-3", "start", ""))
	if line := progress.Apply(event(clock, "writer", "pid-3", "exit", "code=1 reason=FAIL retryable=false")); line != "  └ writer failed code=1 reason=FAIL retryable=false" {
		t.Fatalf("non-zero exit should read as a failure, got %q", line)
	}
	progress.Apply(event(clock, "writer", "pid-4", "start", ""))
	if line := progress.Apply(event(clock, "writer", "pid-4", "exit", "code=0 reason=done retryable=false")); !strings.HasPrefix(line, "  └ writer exited") {
		t.Fatalf("zero exit should read as a normal exit, got %q", line)
	}
}

func TestWorkflowProgressShowsVerdictAndStaleness(t *testing.T) {
	clock := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	progress := NewWorkflowProgress(Palette{}, agentflow.WorkflowSpec{
		Name:       "gated",
		EntryAgent: "coordinator",
		Agents: []agentflow.AgentSpec{
			{ID: "coordinator", Capabilities: capability.Set{Tools: []string{"write_file"}}},
			{ID: "writer", Capabilities: capability.Set{Tools: []string{"write_file"}}},
			{ID: "verifier", Capabilities: capability.Set{Tools: []string{"read_file"}}},
		},
		Verification: &agentflow.VerificationSpec{Policy: "required", Verifier: "verifier"},
	})
	progress.now = func() time.Time { return clock }
	step := func(agent, pid, detail string) {
		progress.Apply(event(clock, agent, pid, "start", ""))
		progress.Apply(event(clock, agent, pid, "exit", detail))
	}
	progress.Apply(event(clock, "coordinator", "pid-2", "start", ""))
	if status := progress.StatusLine(); !strings.Contains(status, "verify: pending") {
		t.Fatalf("expected a pending verdict, got %q", status)
	}
	step("writer", "pid-3", "code=0 reason=wrote retryable=false")
	step("verifier", "pid-4", "code=1 reason=FAIL retryable=false")
	if status := progress.StatusLine(); !strings.Contains(status, "verify: FAIL (pid-4)") {
		t.Fatalf("expected a failed verdict, got %q", status)
	}
	step("writer", "pid-5", "code=0 reason=fixed retryable=false")
	if status := progress.StatusLine(); !strings.Contains(status, "verify: stale (writer ran after pid-4)") {
		t.Fatalf("a writer after the verdict makes it stale, got %q", status)
	}
	step("verifier", "pid-6", "code=0 reason=PASS retryable=false")
	progress.Observe(event(clock, "coordinator", "pid-2", "exit", "code=0 reason=done retryable=false"))
	if status := progress.StatusLine(); !strings.Contains(status, "idle") || !strings.Contains(status, "verify: pass (pid-6)") {
		t.Fatalf("the entry agent's own exit must not mark the verdict stale, got %q", status)
	}
}
