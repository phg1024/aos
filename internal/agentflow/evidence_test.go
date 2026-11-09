package agentflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/replay"
	"aos/internal/runtime"
	"aos/internal/statedir"
	"aos/internal/supervisor"
	"aos/internal/tools"
)

type step = func(runtime.GenerateRequest) runtime.GenerateResult

type evidenceFixture struct {
	state     string
	kernel    *kernel.Kernel
	store     *artifact.Store
	leases    *lease.Manager
	bus       *ipc.Bus
	workspace string
	record    string
	newer     string
}

func newEvidenceFixture(t *testing.T) *evidenceFixture {
	t.Helper()
	state := t.TempDir()
	k, err := kernel.New(filepath.Join(state, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(filepath.Join(state, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	leases, err := lease.New(filepath.Join(state, "leases"), k)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := ipc.New(filepath.Join(state, "bus"), k)
	if err != nil {
		t.Fatal(err)
	}
	workspace, _ := filepath.EvalSymlinks(t.TempDir())
	f := &evidenceFixture{state: state, kernel: k, store: store, leases: leases, bus: bus, workspace: workspace,
		record: filepath.Join(workspace, "record.txt"), newer: filepath.Join(workspace, "newer.txt")}
	if err := os.WriteFile(f.record, []byte(strings.Repeat("trial line\n", 7000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.newer, []byte(strings.Repeat("later trial\n", 7000)), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// spec: a coordinator, a benchmarker that only publishes, so it cannot change the
// work, a verifier and a changer that can.
func (f *evidenceFixture) spec(evidence []agentflow.EvidenceSpec, verifierSteps int, restart *supervisor.RestartPolicy) agentflow.WorkflowSpec {
	mounts := []capability.Mount{{Name: tools.WorkspaceMount, HostPath: f.workspace}}
	read := capability.Set{Tools: []string{"publish_artifact", "read_file"}, ReadPaths: []string{f.workspace}}
	return agentflow.WorkflowSpec{
		Name:         "evidence",
		EntryAgent:   "coordinator",
		Verification: &agentflow.VerificationSpec{Policy: "required", Verifier: "verifier", Evidence: evidence},
		Agents: []agentflow.AgentSpec{
			{ID: "coordinator", SystemPrompt: "Delegate.", AllowedAgents: []string{"benchmarker", "verifier", "changer"}, Mounts: mounts,
				Capabilities: capability.Set{Tools: []string{"spawn_agent", "message_agent", "publish_artifact", "read_file", "write_file"}, ReadPaths: []string{f.workspace}, WritePaths: []string{f.workspace}}},
			{ID: "benchmarker", SystemPrompt: "Publish.", MaxSteps: 6, Mounts: mounts, Capabilities: read},
			{ID: "verifier", SystemPrompt: "Verify.", MaxSteps: verifierSteps, Restart: restart, Mounts: mounts, Capabilities: capability.Set{Tools: []string{"read_file"}, ReadPaths: []string{f.workspace}}},
			{ID: "changer", SystemPrompt: "Change.", MaxSteps: 4, Mounts: mounts, Capabilities: capability.Set{Tools: []string{"write_file"}, ReadPaths: []string{f.workspace}, WritePaths: []string{f.workspace}}},
		},
	}
}

func (f *evidenceFixture) engine(backends scripts) agentflow.Engine {
	return agentflow.Engine{Kernel: f.kernel, Artifacts: f.store, Leases: f.leases, Bus: f.bus, Tools: tools.NewRuntime(), Factory: backends,
		Sleep: func(context.Context, time.Duration) {}}
}

var rawRecord = []agentflow.EvidenceSpec{{Publisher: "benchmarker", Type: "raw_record", FromFile: true}}

// latest is the newest raw record so far.
func (f *evidenceFixture) latest() string {
	records, err := f.store.List()
	if err != nil {
		panic(err)
	}
	var id string
	for _, record := range records {
		if record.Type == "published.raw_record" && statedir.Newer(record.ID, id) {
			id = record.ID
		}
	}
	return id
}

func (f *evidenceFixture) all() string {
	records, err := f.store.List()
	if err != nil {
		panic(err)
	}
	var ids []string
	for _, record := range records {
		if record.Type == "published.raw_record" {
			ids = append(ids, record.ID)
		}
	}
	slices.SortFunc(ids, func(a, b string) int {
		if statedir.Newer(a, b) {
			return 1
		}
		return -1
	})
	return strings.Join(ids, ",")
}

// delegateAttaching delegates with handles it reads when the step comes.
func delegateAttaching(agentID string, handles func() string) step {
	return func(runtime.GenerateRequest) runtime.GenerateResult {
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "delegate", Delegate: &runtime.DelegateCall{AgentID: agentID, Goal: "check the work", Artifacts: handles()}}}
	}
}

func spawnAttaching(agentID string, handles func() string) step {
	return func(runtime.GenerateRequest) runtime.GenerateResult {
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: "spawn_agent", Args: map[string]string{"agent_id": agentID, "goal": "check the work", "artifacts": handles()}}}}
	}
}

// page reads part of the latest record, or whichever handles() names.
func page(handle func() string, offset string, limit string) step {
	return func(runtime.GenerateRequest) runtime.GenerateResult {
		args := map[string]string{"id": handle(), "offset": offset}
		if limit != "" {
			args["limit"] = limit
		}
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: "read_artifact", Args: args}}}
	}
}

func (f *evidenceFixture) wholeRead() []step {
	return []step{page(f.latest, "0", "50000"), page(f.latest, "50000", "")}
}

func pass() step { return exitStep(0, false, "PASS") }

func attempts(n int) []step {
	var out []step
	for range n {
		out = append(out, pass())
	}
	return out
}

func noop(n int) []step {
	var out []step
	for i := range n {
		out = append(out, toolStep("read_file", map[string]string{"path": fmt.Sprintf("/nowhere/%d", i)}))
	}
	return out
}

func joined(lists ...[]step) []step { return slices.Concat(lists...) }

var (
	publishFile = func(f *evidenceFixture) step {
		return toolStep("publish_artifact", map[string]string{"type": "raw_record", "path": f.record})
	}
	publishNewer = func(f *evidenceFixture) step {
		return toolStep("publish_artifact", map[string]string{"type": "raw_record", "path": f.newer})
	}
	publishTyped = toolStep("publish_artifact", map[string]string{"type": "raw_record", "body": "typed by a model"})
	finished     = exitStep(0, false, "done")
)

type evidenceScenario struct {
	name                               string
	coordinator, benchmarker, verifier func(*evidenceFixture) []step
	changer                            func(*evidenceFixture) []step
	verifierSteps                      int
	status, evidence                   string
	refused                            int
	// artifactIndex is which raw record the check should name, from 0; -1 for none.
	artifactIndex int
}

func evidenceScenarios() []evidenceScenario {
	delegateAll := func(f *evidenceFixture) []step {
		return []step{delegateStep("benchmarker"), delegateAttaching("verifier", f.latest), finished}
	}
	publishes := func(f *evidenceFixture) []step { return []step{publishFile(f), finished} }
	return []evidenceScenario{
		{
			name: "a pass with everything read", status: agentflow.VerdictPass, evidence: agentflow.EvidenceRead,
			coordinator: delegateAll, benchmarker: publishes,
			verifier: func(f *evidenceFixture) []step { return joined(f.wholeRead(), []step{pass()}) },
		},
		{
			name: "pages out of order still join", status: agentflow.VerdictPass, evidence: agentflow.EvidenceRead,
			coordinator: delegateAll, benchmarker: publishes,
			verifier: func(f *evidenceFixture) []step {
				return []step{page(f.latest, "50000", ""), page(f.latest, "0", "50000"), pass()}
			},
		},
		{
			name: "a read of an older record", status: agentflow.VerdictUnverified, evidence: agentflow.EvidenceUnread, refused: 3, artifactIndex: 1,
			coordinator: func(f *evidenceFixture) []step {
				return []step{delegateStep("benchmarker"), delegateAttaching("verifier", f.all), finished}
			},
			benchmarker: func(f *evidenceFixture) []step { return []step{publishFile(f), publishNewer(f), finished} },
			verifier: func(f *evidenceFixture) []step {
				older := func() string { return strings.Split(f.all(), ",")[0] }
				return joined([]step{page(older, "0", "50000"), page(older, "50000", "")}, attempts(3))
			},
			verifierSteps: 5,
		},
		{
			name: "a read by another verifier", status: agentflow.VerdictUnverified, evidence: agentflow.EvidenceUnread, refused: 3,
			coordinator: func(f *evidenceFixture) []step {
				return []step{delegateStep("benchmarker"), delegateAttaching("verifier", f.latest), delegateAttaching("verifier", f.latest), finished}
			},
			benchmarker: publishes,
			verifier: func(f *evidenceFixture) []step {
				return joined(f.wholeRead(), []step{pass()}, attempts(3))
			},
			verifierSteps: 3,
		},
		{
			name: "a re-delegated chain", status: agentflow.VerdictUnverified, evidence: agentflow.EvidenceUnread, refused: 3,
			coordinator: func(f *evidenceFixture) []step {
				return []step{delegateStep("benchmarker"), delegateAttaching("verifier", f.latest), delegateAttaching("verifier", f.latest), finished}
			},
			benchmarker: publishes,
			// The first runs out of steps having read it all. The second is a new chain.
			verifier:      func(f *evidenceFixture) []step { return joined(f.wholeRead(), noop(1), attempts(3)) },
			verifierSteps: 3,
		},
		{
			name: "a partial read", status: agentflow.VerdictUnverified, evidence: agentflow.EvidencePartial, refused: 3,
			coordinator: delegateAll, benchmarker: publishes,
			verifier:      func(f *evidenceFixture) []step { return joined([]step{page(f.latest, "0", "50000")}, attempts(3)) },
			verifierSteps: 4,
		},
		{
			name: "the last page alone", status: agentflow.VerdictUnverified, evidence: agentflow.EvidencePartial, refused: 3,
			coordinator: delegateAll, benchmarker: publishes,
			verifier:      func(f *evidenceFixture) []step { return joined([]step{page(f.latest, "50000", "")}, attempts(3)) },
			verifierSteps: 4,
		},
		{
			name: "a record never published", status: agentflow.VerdictUnverified, evidence: agentflow.EvidenceNotPublished, refused: 3, artifactIndex: -1,
			coordinator: delegateAll, benchmarker: func(*evidenceFixture) []step { return []step{finished} },
			verifier:      func(*evidenceFixture) []step { return attempts(3) },
			verifierSteps: 3,
		},
		{
			name: "a record typed when a file is required", status: agentflow.VerdictUnverified, evidence: agentflow.EvidenceNotFromFile, refused: 3,
			coordinator: delegateAll, benchmarker: func(*evidenceFixture) []step { return []step{publishTyped, finished} },
			verifier: func(f *evidenceFixture) []step {
				return joined([]step{page(f.latest, "0", "")}, attempts(3))
			},
			verifierSteps: 4,
		},
		{
			name: "a step limit with nothing read", status: agentflow.VerdictUnverified, evidence: agentflow.EvidenceUnread,
			coordinator: delegateAll, benchmarker: publishes,
			verifier:      func(*evidenceFixture) []step { return noop(3) },
			verifierSteps: 3,
		},
		{
			name: "a step limit with everything read, and no judgement", status: agentflow.VerdictUnverified, evidence: agentflow.EvidenceRead,
			coordinator: delegateAll, benchmarker: publishes,
			verifier:      func(f *evidenceFixture) []step { return joined(f.wholeRead(), noop(1)) },
			verifierSteps: 3,
		},
		{
			name: "pid-000009's path: refused, then tries to exit 0", status: agentflow.VerdictUnverified, evidence: agentflow.EvidenceUnread, refused: 4,
			coordinator: func(*evidenceFixture) []step {
				return []step{delegateStep("benchmarker"), delegateStep("verifier"), finished}
			},
			benchmarker: publishes,
			verifier: func(f *evidenceFixture) []step {
				return joined([]step{page(f.latest, "0", "")}, attempts(4))
			},
			verifierSteps: 5,
		},
		{
			name: "the wait route stays: mail, then resume", status: agentflow.VerdictPass, evidence: agentflow.EvidenceRead,
			coordinator: func(f *evidenceFixture) []step {
				return []step{delegateStep("benchmarker"), delegateStep("verifier"), toolStepFn("message_agent", func() map[string]string {
					return map[string]string{"to": verifierPID(f), "body": "the record", "artifacts": f.latest()}
				}), delegateStep("verifier"), finished}
			},
			benchmarker: publishes,
			verifier: func(f *evidenceFixture) []step {
				return joined([]step{page(f.latest, "0", ""), waitStep("I need the record")}, f.wholeRead(), []step{pass()})
			},
		},
		{
			name: "a handle attached when resuming a parked verifier", status: agentflow.VerdictPass, evidence: agentflow.EvidenceRead,
			coordinator: func(f *evidenceFixture) []step {
				return []step{delegateStep("benchmarker"), delegateStep("verifier"), delegateAttaching("verifier", f.latest), finished}
			},
			benchmarker: publishes,
			verifier: func(f *evidenceFixture) []step {
				return joined([]step{waitStep("I need the record")}, f.wholeRead(), []step{pass()})
			},
		},
		{
			name: "a verifier's own failure stands", status: agentflow.VerdictFail, evidence: agentflow.EvidenceUnread,
			coordinator: delegateAll, benchmarker: publishes,
			verifier: func(*evidenceFixture) []step { return []step{exitStep(1, false, "FAIL")} },
		},
		{
			name: "a new record after an accepted exit", status: agentflow.VerdictUnverified, evidence: agentflow.EvidenceUnread, artifactIndex: 1,
			coordinator: func(f *evidenceFixture) []step {
				return []step{delegateStep("benchmarker"), delegateAttaching("verifier", f.latest), delegateStep("benchmarker"), finished}
			},
			benchmarker: func(f *evidenceFixture) []step {
				return []step{publishFile(f), finished, publishNewer(f), finished}
			},
			verifier: func(f *evidenceFixture) []step { return joined(f.wholeRead(), []step{pass()}) },
		},
		{
			name: "work changed after the verifier", status: agentflow.VerdictStale, evidence: agentflow.EvidenceRead,
			coordinator: func(f *evidenceFixture) []step {
				return []step{delegateStep("benchmarker"), delegateAttaching("verifier", f.latest), delegateStep("changer"), finished}
			},
			benchmarker: publishes,
			verifier:    func(f *evidenceFixture) []step { return joined(f.wholeRead(), []step{pass()}) },
			changer:     func(*evidenceFixture) []step { return []step{finished} },
		},
	}
}

func toolStepFn(name string, args func() map[string]string) step {
	return func(runtime.GenerateRequest) runtime.GenerateResult {
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: name, Args: args()}}}
	}
}

func verifierPID(f *evidenceFixture) string {
	processes, err := f.kernel.Processes()
	if err != nil {
		panic(err)
	}
	for _, process := range processes {
		if process.Metadata["agent_id"] == "verifier" {
			return process.PID
		}
	}
	return ""
}

func (f *evidenceFixture) rawRecords(t *testing.T) []string {
	t.Helper()
	records, err := f.store.List()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, record := range records {
		if record.Type == "published.raw_record" {
			ids = append(ids, record.ID)
		}
	}
	slices.SortFunc(ids, func(a, b string) int {
		if statedir.Newer(a, b) {
			return 1
		}
		return -1
	})
	return ids
}

func (s evidenceScenario) run(t *testing.T, f *evidenceFixture) (agentflow.WorkflowSpec, agentflow.RunSummary) {
	t.Helper()
	steps := s.verifierSteps
	if steps == 0 {
		steps = 8
	}
	spec := f.spec(rawRecord, steps, nil)
	backends := scripts{
		"coordinator": {replies: s.coordinator(f)},
		"benchmarker": {replies: s.benchmarker(f)},
		"verifier":    {replies: s.verifier(f)},
	}
	if s.changer != nil {
		backends["changer"] = &script{replies: s.changer(f)}
	}
	engine := f.engine(backends)
	summary, err := engine.Run(context.Background(), spec, agentflow.RunRequest{Goal: "verify the work"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if summary.Verification == nil {
		t.Fatal("no verdict")
	}
	return spec, summary
}

func (f *evidenceFixture) refusals(t *testing.T) []kernel.Event {
	t.Helper()
	events, err := f.kernel.Events()
	if err != nil {
		t.Fatal(err)
	}
	var out []kernel.Event
	for _, event := range events {
		if event.Type == agentflow.ExitRefusedEvent {
			out = append(out, event)
		}
	}
	return out
}

var declaredLine = regexp.MustCompile(`(?m)^declared (\S+) by (\S+): (\S+): (\S+)$`)

// Every scenario: the verdict, what the check names, how many exits were refused, and
// that the verdict and analyze.py agree on the same records. So do the verdict and a
// second judgement from those records, since a judgement must not depend on the model
// or on how the run got there.
func TestAPassIsBoundToTheDeclaredEvidence(t *testing.T) {
	for _, scenario := range evidenceScenarios() {
		t.Run(scenario.name, func(t *testing.T) {
			f := newEvidenceFixture(t)
			spec, summary := scenario.run(t, f)
			verdict := *summary.Verification
			if verdict.Status != scenario.status {
				t.Fatalf("status %q, want %q: %s", verdict.Status, scenario.status, verdict.Reason)
			}
			if len(verdict.Evidence) != 1 || verdict.Evidence[0].Status != scenario.evidence {
				t.Fatalf("evidence %+v, want %s", verdict.Evidence, scenario.evidence)
			}
			check := verdict.Evidence[0]
			if check.Publisher != "benchmarker" || check.Type != "raw_record" {
				t.Fatalf("check names %+v", check)
			}
			records := f.rawRecords(t)
			switch {
			case scenario.artifactIndex < 0 && check.ArtifactID != "":
				t.Fatalf("named %s, published nothing", check.ArtifactID)
			case scenario.artifactIndex >= 0 && (scenario.artifactIndex >= len(records) || check.ArtifactID != records[scenario.artifactIndex]):
				t.Fatalf("named %s of %v, want index %d", check.ArtifactID, records, scenario.artifactIndex)
			}
			if got := len(f.refusals(t)); got != scenario.refused {
				t.Fatalf("%d exits refused, want %d", got, scenario.refused)
			}
			if scenario.status == agentflow.VerdictUnverified {
				if summary.ExitStatus.Code == 0 || !summary.ExitStatus.Retryable {
					t.Fatalf("an unverified run exited %+v", summary.ExitStatus)
				}
				if !strings.Contains(verdict.Reason, "says nothing about the work") {
					t.Fatalf("reason %q could read as a judgement of the work", verdict.Reason)
				}
			}

			engine := f.engine(nil)
			policy, err := spec.VerificationPolicy()
			if err != nil {
				t.Fatal(err)
			}
			again, err := engine.Judge(spec, agentflow.RunRequest{Goal: "verify the work"}, policy, summary.RootPID, summary.EntryPID)
			if err != nil {
				t.Fatal(err)
			}
			first, _ := json.Marshal(verdict)
			second, _ := json.Marshal(again)
			if string(first) != string(second) {
				t.Fatalf("judged again:\n%s\nfirst:\n%s", second, first)
			}

			out := runAnalyzer(t, f.state)
			if strings.Contains(out, "DISAGREE") {
				t.Fatalf("the analyzer disagrees:\n%s", out)
			}
			found := declaredLine.FindAllStringSubmatch(out, -1)
			if len(found) != 1 {
				t.Fatalf("analyzer lines %v:\n%s", found, out)
			}
			artifactID := found[0][3]
			if artifactID == "-" {
				artifactID = ""
			}
			if found[0][1] != check.Type || found[0][2] != check.Publisher || artifactID != check.ArtifactID || found[0][4] != check.Status {
				t.Fatalf("analyzer says %v, the verdict %+v", found[0][1:], check)
			}
			if scenario.status == agentflow.VerdictUnverified && !strings.Contains(out, "UNVERIFIED:") {
				t.Fatalf("no UNVERIFIED line:\n%s", out)
			}
			if want := fmt.Sprintf("exits refused for evidence: %d", scenario.refused); !strings.Contains(out, want) {
				t.Fatalf("no %q:\n%s", want, out)
			}
		})
	}
}

func runAnalyzer(t *testing.T, state string) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "reports", "ipc-live", "analyze.py"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-I", script, state)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 3) {
		t.Fatalf("analyze.py: %v\n%s", err, out)
	}
	return string(out)
}

// The model reads why, while it has steps left, and what would get it past.
func TestARefusedExitTellsTheModelWhatToDo(t *testing.T) {
	for _, tc := range []struct {
		name      string
		benchmark func(*evidenceFixture) []step
		verifier  func(*evidenceFixture) []step
		want      []string
	}{
		{"unread", func(f *evidenceFixture) []step { return []step{publishFile(f), finished} }, func(*evidenceFixture) []step { return attempts(2) },
			[]string{"tool_error name=exit", "exit 0 refused", "read_artifact", "wait", "unverified"}},
		{"partial", func(f *evidenceFixture) []step { return []step{publishFile(f), finished} }, func(f *evidenceFixture) []step {
			return joined([]step{page(f.latest, "0", "50000")}, attempts(2))
		}, []string{"exit 0 refused", "in part"}},
		{"never published", func(*evidenceFixture) []step { return []step{finished} }, func(*evidenceFixture) []step { return attempts(2) },
			[]string{"exit 0 refused", "has published no raw_record", "wait"}},
		{"typed", func(*evidenceFixture) []step { return []step{publishTyped, finished} }, func(*evidenceFixture) []step { return attempts(2) },
			[]string{"exit 0 refused", "not from a file", "wait"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEvidenceFixture(t)
			verifier := &script{replies: tc.verifier(f)}
			engine := f.engine(scripts{
				"coordinator": {replies: []step{delegateStep("benchmarker"), delegateAttaching("verifier", f.latest), finished}},
				"benchmarker": {replies: tc.benchmark(f)},
				"verifier":    verifier,
			})
			steps := len(verifier.replies)
			if _, err := engine.Run(context.Background(), f.spec(rawRecord, steps, nil), agentflow.RunRequest{Goal: "verify"}); err != nil {
				t.Fatal(err)
			}
			seen := verifier.requests()
			var history strings.Builder
			for _, message := range seen[len(seen)-1].History {
				history.WriteString(message.Content + "\n")
			}
			for _, want := range tc.want {
				if !strings.Contains(history.String(), want) {
					t.Fatalf("lacks %q:\n%s", want, history.String())
				}
			}
		})
	}
}

// A nonzero exit is never refused: it claims no pass.
func TestANonzeroExitIsNotRefused(t *testing.T) {
	f := newEvidenceFixture(t)
	engine := f.engine(scripts{
		"coordinator": {replies: []step{delegateStep("benchmarker"), delegateStep("verifier"), finished}},
		"benchmarker": {replies: []step{publishFile(f), finished}},
		"verifier":    {replies: []step{exitStep(1, false, "FAIL")}},
	})
	if _, err := engine.Run(context.Background(), f.spec(rawRecord, 2, nil), agentflow.RunRequest{Goal: "verify"}); err != nil {
		t.Fatal(err)
	}
	if got := f.refusals(t); len(got) != 0 {
		t.Fatalf("refused: %+v", got)
	}
}

// Only the declared verifier's exit is checked.
func TestOnlyTheVerifiersExitIsChecked(t *testing.T) {
	f := newEvidenceFixture(t)
	engine := f.engine(scripts{
		"coordinator": {replies: []step{delegateStep("benchmarker"), finished}},
		"benchmarker": {replies: []step{finished}},
	})
	summary, err := engine.Run(context.Background(), f.spec(rawRecord, 2, nil), agentflow.RunRequest{Goal: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.refusals(t); len(got) != 0 {
		t.Fatalf("refused: %+v", got)
	}
	if summary.Verification.Status != agentflow.VerdictMissing || len(summary.Verification.Evidence) != 0 {
		t.Fatalf("verdict %+v", summary.Verification)
	}
}

// A declaration that does not ask for a file takes a typed record, read to its end.
func TestATypedRecordPassesWhenNoFileIsRequired(t *testing.T) {
	f := newEvidenceFixture(t)
	engine := f.engine(scripts{
		"coordinator": {replies: []step{delegateStep("benchmarker"), delegateAttaching("verifier", f.latest), finished}},
		"benchmarker": {replies: []step{publishTyped, finished}},
		"verifier":    {replies: []step{page(f.latest, "0", ""), pass()}},
	})
	declared := []agentflow.EvidenceSpec{{Publisher: "benchmarker", Type: "raw_record"}}
	summary, err := engine.Run(context.Background(), f.spec(declared, 4, nil), agentflow.RunRequest{Goal: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Verification.Status != agentflow.VerdictPass || summary.Verification.Evidence[0].Status != agentflow.EvidenceRead {
		t.Fatalf("verdict %+v", summary.Verification)
	}
	if out := runAnalyzer(t, f.state); strings.Contains(out, "DISAGREE") {
		t.Fatalf("the analyzer disagrees:\n%s", out)
	}
}

// A workflow that declares nothing passes as it did, with nothing in the verdict.
func TestDeclaringNothingChangesNothing(t *testing.T) {
	f := newEvidenceFixture(t)
	engine := f.engine(scripts{
		"coordinator": {replies: []step{delegateStep("benchmarker"), delegateStep("verifier"), finished}},
		"benchmarker": {replies: []step{publishFile(f), finished}},
		"verifier":    {replies: []step{pass()}},
	})
	summary, err := engine.Run(context.Background(), f.spec(nil, 2, nil), agentflow.RunRequest{Goal: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Verification.Status != agentflow.VerdictPass || summary.ExitStatus.Code != 0 || len(f.refusals(t)) != 0 {
		t.Fatalf("verdict %+v, exit %+v", summary.Verification, summary.ExitStatus)
	}
	encoded, _ := json.Marshal(summary.Verification)
	if strings.Contains(string(encoded), "evidence") {
		t.Fatalf("verdict %s", encoded)
	}
	// And a step limit is still a failure there.
	f = newEvidenceFixture(t)
	engine = f.engine(scripts{
		"coordinator": {replies: []step{delegateStep("verifier"), finished}},
		"verifier":    {replies: noop(1)},
	})
	summary, err = engine.Run(context.Background(), f.spec(nil, 1, nil), agentflow.RunRequest{Goal: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Verification.Status != agentflow.VerdictFail {
		t.Fatalf("verdict %+v", summary.Verification)
	}
}

// outsider publishes outside the coordinator's reach.
func (f *evidenceFixture) outsider(t *testing.T) string {
	t.Helper()
	process, err := f.kernel.Spawn(kernel.ProcessSpec{Role: "outsider"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := f.store.CreateJSON("published.secret", process.PID, map[string]string{"body": "not yours"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return record.ID
}

func (f *evidenceFixture) grants(t *testing.T) []artifact.Artifact {
	t.Helper()
	records, err := f.store.List()
	if err != nil {
		t.Fatal(err)
	}
	var out []artifact.Artifact
	for _, record := range records {
		if record.Type == runtime.DelegationHandlesArtifact {
			out = append(out, record)
		}
	}
	return out
}

func toolErrorsOf(requests []runtime.GenerateRequest) string {
	if len(requests) == 0 {
		return ""
	}
	var out strings.Builder
	for _, message := range requests[len(requests)-1].History {
		if strings.Contains(message.Content, "tool_error") {
			out.WriteString(message.Content + "\n")
		}
	}
	return out.String()
}

// Attaching what the delegator cannot read refuses the whole delegation, with a tool
// error it reads and goes on from. Nothing starts, and nothing is granted.
func TestAnAttachmentBeyondTheDelegatorsAuthorityIsRefused(t *testing.T) {
	for _, route := range []struct {
		name string
		step func(string, func() string) step
	}{{"delegate", delegateAttaching}, {"spawn_agent", spawnAttaching}} {
		for _, tc := range []struct {
			name    string
			handles func(*evidenceFixture, *testing.T) string
		}{
			{"another chain's", func(f *evidenceFixture, t *testing.T) string { return f.outsider(t) }},
			{"one good, one not", func(f *evidenceFixture, t *testing.T) string { return f.latest() + "," + f.outsider(t) }},
			{"no such artifact", func(*evidenceFixture, *testing.T) string { return "art-999999" }},
			{"a runtime record", func(f *evidenceFixture, t *testing.T) string {
				records, err := f.store.List()
				if err != nil {
					t.Fatal(err)
				}
				at := slices.IndexFunc(records, func(record artifact.Artifact) bool { return record.Type == "tool_result" })
				return records[at].ID
			}},
		} {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				f := newEvidenceFixture(t)
				coordinator := &script{replies: []step{delegateStep("benchmarker"), route.step("verifier", func() string { return tc.handles(f, t) }), finished}}
				engine := f.engine(scripts{"coordinator": coordinator, "benchmarker": {replies: []step{publishFile(f), finished}}})
				summary, err := engine.Run(context.Background(), f.spec(nil, 2, nil), agentflow.RunRequest{Goal: "verify"})
				if err != nil {
					t.Fatal(err)
				}
				if verifierPID(f) != "" {
					t.Fatal("the delegation started")
				}
				if got := f.grants(t); len(got) != 0 {
					t.Fatalf("granted %+v", got)
				}
				if errs := toolErrorsOf(coordinator.requests()); !strings.Contains(errs, runtime.ErrArtifactNotReadable.Error()) {
					t.Fatalf("the model read: %q", errs)
				}
				if summary.Verification.Status != agentflow.VerdictMissing {
					t.Fatalf("verdict %+v", summary.Verification)
				}
				events, err := f.kernel.Events()
				if err != nil {
					t.Fatal(err)
				}
				if !slices.ContainsFunc(events, func(event kernel.Event) bool {
					return event.Type == "capability.denied" && event.Data["capability"] == "read_artifact"
				}) {
					t.Fatal("no denial logged")
				}
			})
		}
	}
}

// A delegator passes on what it holds through mail too: a handle it received at its
// own delegation can go on to a child, and this is the one check.
func TestAHandleAtDelegationIsReadFromTheFirstStep(t *testing.T) {
	for _, route := range []struct {
		name string
		step func(string, func() string) step
	}{{"delegate", delegateAttaching}, {"spawn_agent", spawnAttaching}} {
		t.Run(route.name, func(t *testing.T) {
			f := newEvidenceFixture(t)
			verifier := &script{replies: joined(f.wholeRead(), []step{pass()})}
			engine := f.engine(scripts{
				"coordinator": {replies: []step{delegateStep("benchmarker"), route.step("verifier", f.latest), finished}},
				"benchmarker": {replies: []step{publishFile(f), finished}},
				"verifier":    verifier,
			})
			summary, err := engine.Run(context.Background(), f.spec(rawRecord, 4, nil), agentflow.RunRequest{Goal: "verify"})
			if err != nil {
				t.Fatal(err)
			}
			if summary.Verification.Status != agentflow.VerdictPass {
				t.Fatalf("verdict %+v", summary.Verification)
			}
			grants := f.grants(t)
			if len(grants) != 1 || grants[0].ProducerPID != verifierPID(f) || grants[0].Metadata["artifacts"] != f.latest() {
				t.Fatalf("grants %+v", grants)
			}
			first := verifier.requests()[0].History[0].Content
			if !strings.Contains(first, f.latest()) || !strings.Contains(first, "published.raw_record") || !strings.Contains(first, "check the work") {
				t.Fatalf("the first message: %q", first)
			}
		})
	}
}

// The right is the new process's chain's: another process of the agent gets nothing
// from it, and neither does a sibling.
func TestAHandleAtDelegationStaysWithItsChain(t *testing.T) {
	f := newEvidenceFixture(t)
	verifier := &script{replies: []step{page(f.latest, "0", "50000"), pass(), page(f.latest, "0", ""), exitStep(1, false, "could not read it")}}
	benchmarker := &script{replies: []step{publishFile(f), finished, page(func() string { return "" }, "0", ""), finished}}
	engine := f.engine(scripts{
		"coordinator": {replies: []step{delegateStep("benchmarker"), delegateAttaching("verifier", f.latest), delegateStep("verifier"), finished}},
		"benchmarker": benchmarker,
		"verifier":    verifier,
	})
	if _, err := engine.Run(context.Background(), f.spec(nil, 4, nil), agentflow.RunRequest{Goal: "verify"}); err != nil {
		t.Fatal(err)
	}
	errs := toolErrorsOf(verifier.requests())
	if strings.Contains(toolErrorsOf(verifier.requests()[:1]), "read_artifact") || !strings.Contains(errs, runtime.ErrArtifactNotReadable.Error()) {
		t.Fatalf("the second verifier's errors: %q", errs)
	}
}

// A process with a handle may attach it onward: rights it holds through delegation
// count as any others do, and no further.
func TestAHandleAtDelegationCanBePassedOn(t *testing.T) {
	f := newEvidenceFixture(t)
	spec := f.spec(nil, 4, nil)
	for i := range spec.Agents {
		if spec.Agents[i].ID == "verifier" {
			spec.Agents[i].Capabilities.Tools = append(spec.Agents[i].Capabilities.Tools, "message_agent")
		}
	}
	coordinator := &script{replies: []step{delegateStep("benchmarker"), delegateAttaching("verifier", f.latest), finished}}
	verifier := &script{replies: []step{toolStepFn("message_agent", func() map[string]string {
		processes, _ := f.kernel.Processes()
		var to string
		for _, process := range processes {
			if process.Metadata["agent_id"] == "coordinator" {
				to = process.PID
			}
		}
		return map[string]string{"to": to, "body": "for you", "artifacts": f.latest()}
	}), pass()}}
	engine := f.engine(scripts{"coordinator": coordinator, "benchmarker": {replies: []step{publishFile(f), finished}}, "verifier": verifier})
	if _, err := engine.Run(context.Background(), spec, agentflow.RunRequest{Goal: "verify"}); err != nil {
		t.Fatal(err)
	}
	if errs := toolErrorsOf(verifier.requests()); errs != "" {
		t.Fatalf("errors: %s", errs)
	}
}

func TestEvidenceSpecsAreChecked(t *testing.T) {
	f := newEvidenceFixture(t)
	for name, tc := range map[string]struct {
		change func(*agentflow.WorkflowSpec)
		want   string
	}{
		"no publisher": {func(s *agentflow.WorkflowSpec) { s.Verification.Evidence = []agentflow.EvidenceSpec{{Type: "x"}} }, "publisher"},
		"unknown publisher": {func(s *agentflow.WorkflowSpec) {
			s.Verification.Evidence = []agentflow.EvidenceSpec{{Publisher: "nobody", Type: "x"}}
		}, "not an agent"},
		"no type": {func(s *agentflow.WorkflowSpec) {
			s.Verification.Evidence = []agentflow.EvidenceSpec{{Publisher: "benchmarker"}}
		}, "type"},
		"the verifier itself": {func(s *agentflow.WorkflowSpec) {
			s.Verification.Evidence = []agentflow.EvidenceSpec{{Publisher: "verifier", Type: "x"}}
		}, "its own"},
		"a publisher that cannot publish": {func(s *agentflow.WorkflowSpec) {
			s.Verification.Evidence = []agentflow.EvidenceSpec{{Publisher: "changer", Type: "x"}}
		}, "publish_artifact"},
		"from a file, with no read_file": {func(s *agentflow.WorkflowSpec) {
			s.Verification.Evidence = []agentflow.EvidenceSpec{{Publisher: "benchmarker", Type: "x", FromFile: true}}
			for i := range s.Agents {
				if s.Agents[i].ID == "benchmarker" {
					s.Agents[i].Capabilities.Tools = []string{"publish_artifact"}
				}
			}
		}, "read_file"},
		"no verifier policy": {func(s *agentflow.WorkflowSpec) { s.Verification.Policy = "none" }, "policy"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := f.spec(rawRecord, 2, nil)
			tc.change(&spec)
			err := spec.Validate()
			if !errors.Is(err, agentflow.ErrWorkflowSpecInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if err := f.spec(rawRecord, 2, nil).Validate(); err != nil {
		t.Fatalf("a good spec: %v", err)
	}
}

// Pages read before a recovery and after it are one reading, as they are one chain:
// the process a recovery spawned reads on from the record of the one it replaces.
func TestAReadAcrossARecoveryIsTheSameChain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		chained bool
		status  string
	}{{"one chain", true, agentflow.EvidenceRead}, {"not one chain", false, agentflow.EvidencePartial}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEvidenceFixture(t)
			spec := f.spec(rawRecord, 8, nil)
			root, err := f.kernel.Spawn(kernel.ProcessSpec{Role: "workflow:evidence"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.CreateJSON(agentflow.RunSpecArtifact, root.PID, agentflow.RunSpec{Spec: spec}, nil, nil); err != nil {
				t.Fatal(err)
			}
			spawn := func(agent string, metadata map[string]string) kernel.Process {
				merged := map[string]string{"agent_id": agent}
				for key, value := range metadata {
					merged[key] = value
				}
				process, err := f.kernel.Spawn(kernel.ProcessSpec{ParentPID: root.PID, Role: "agent:" + agent, Metadata: merged})
				if err != nil {
					t.Fatal(err)
				}
				return process
			}
			publisher := spawn("benchmarker", nil)
			record, err := f.store.CreateJSON("published.raw_record", publisher.PID, map[string]string{"body": "bytes"}, nil, map[string]string{"kind": "published", "source": "file"})
			if err != nil {
				t.Fatal(err)
			}
			first := spawn("verifier", nil)
			link := map[string]string{}
			if tc.chained {
				link["resumed_from"] = first.PID
			}
			second := spawn("verifier", link)
			read := func(pid string, output map[string]string) {
				output["artifact_id"] = record.ID
				if _, err := f.store.CreateJSON("tool_result", pid, tools.Result{Tool: "read_artifact", Output: output}, nil, map[string]string{"tool": "read_artifact"}); err != nil {
					t.Fatal(err)
				}
			}
			read(first.PID, map[string]string{"offset": "0", "next_offset": "50000"})
			read(second.PID, map[string]string{"offset": "50000"})
			for _, process := range []kernel.Process{publisher, first, second} {
				if _, err := f.kernel.Exit(process.PID, kernel.ExitStatus{Code: 0, Reason: "done"}); err != nil {
					t.Fatal(err)
				}
			}
			engine := f.engine(nil)
			checks, err := engine.EvidenceChecks(spec, second.PID)
			if err != nil {
				t.Fatal(err)
			}
			if len(checks) != 1 || checks[0].Status != tc.status || checks[0].ArtifactID != record.ID {
				t.Fatalf("checks %+v", checks)
			}
			if _, err := f.store.CreateJSON("verification", root.PID, agentflow.Verdict{Status: agentflow.VerdictPass, VerifierPID: second.PID, Evidence: checks}, nil, map[string]string{"verifier_pid": second.PID}); err != nil {
				t.Fatal(err)
			}
			out := runAnalyzer(t, f.state)
			if want := fmt.Sprintf("declared raw_record by benchmarker: %s: %s", record.ID, tc.status); !strings.Contains(out, want) {
				t.Fatalf("no %q:\n%s", want, out)
			}
		})
	}
}

// Before bounds and handles there is nothing to attach: a replay of such a record
// grants nothing, whatever a model wrote.
func TestNoBusBoundsAttachesNothing(t *testing.T) {
	f := newEvidenceFixture(t)
	f.bus.NoBounds = true
	verifier := &script{replies: []step{page(f.latest, "0", ""), exitStep(1, false, "no handle")}}
	engine := f.engine(scripts{
		"coordinator": {replies: []step{delegateStep("benchmarker"), delegateAttaching("verifier", f.latest), finished}},
		"benchmarker": {replies: []step{publishFile(f), finished}},
		"verifier":    verifier,
	})
	engine.NoBusBounds = true
	if _, err := engine.Run(context.Background(), f.spec(nil, 4, nil), agentflow.RunRequest{Goal: "verify"}); err != nil {
		t.Fatal(err)
	}
	if got := f.grants(t); len(got) != 0 {
		t.Fatalf("granted %+v", got)
	}
}

// A record from another workflow root is not this run's evidence, and neither is a
// record of the declared type from another agent.
func TestEvidenceIsThisRunsAndThePublishers(t *testing.T) {
	f := newEvidenceFixture(t)
	spec := f.spec(rawRecord, 8, nil)
	spawn := func(parent, agent string) kernel.Process {
		process, err := f.kernel.Spawn(kernel.ProcessSpec{ParentPID: parent, Role: "agent:" + agent, Metadata: map[string]string{"agent_id": agent}})
		if err != nil {
			t.Fatal(err)
		}
		return process
	}
	here, elsewhere := spawn("", "root"), spawn("", "root")
	publish := func(pid string) string {
		record, err := f.store.CreateJSON("published.raw_record", pid, map[string]string{"body": "bytes"}, nil, map[string]string{"kind": "published", "source": "file"})
		if err != nil {
			t.Fatal(err)
		}
		return record.ID
	}
	verifier := spawn(here.PID, "verifier")
	ours := publish(spawn(here.PID, "benchmarker").PID)
	publish(spawn(elsewhere.PID, "benchmarker").PID)
	publish(spawn(here.PID, "changer").PID)
	engine := f.engine(nil)
	checks, err := engine.EvidenceChecks(spec, verifier.PID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || checks[0].ArtifactID != ours {
		t.Fatalf("checks %+v, want %s", checks, ours)
	}
}

// Replay handles single-agent runs, and a verifier cannot be the entry agent, so no run
// that declares evidence can be replayed. It says so, rather than replaying part of it.
func TestARunThatDeclaresEvidenceIsRefusedByReplay(t *testing.T) {
	f := newEvidenceFixture(t)
	scenario := evidenceScenarios()[0]
	scenario.run(t, f)
	_, err := replay.Run(context.Background(), replay.Options{StateDir: f.state})
	if !errors.Is(err, replay.ErrNotReplayable) || !strings.Contains(err.Error(), "single-agent") {
		t.Fatalf("err = %v", err)
	}
}
