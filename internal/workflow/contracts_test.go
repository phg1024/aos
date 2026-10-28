package workflow_test

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"

	"aos/internal/artifact"
	"aos/internal/kernel"
	"aos/internal/workflow"
)

type fakePlanner struct {
	plan workflow.Plan
}

func (f fakePlanner) Plan(_ context.Context, goal string) (workflow.Plan, error) {
	return f.plan, nil
}

type fakeWorker struct{}

func (fakeWorker) Execute(_ context.Context, contract workflow.TaskContract, attempt int) (workflow.WorkerOutput, error) {
	return workflow.WorkerOutput{
		Summary: "attempt=" + strconv.Itoa(attempt) + " " + contract.Objective,
		Metadata: map[string]string{
			"attempt": strconv.Itoa(attempt),
		},
	}, nil
}

type fakeVerifier struct {
	seen map[string]int
}

func (f *fakeVerifier) Verify(_ context.Context, contract workflow.TaskContract, output workflow.WorkerOutput) (workflow.Verification, error) {
	if f.seen == nil {
		f.seen = map[string]int{}
	}
	f.seen[contract.ID]++
	if contract.ID == "task-1" && f.seen[contract.ID] == 1 {
		return workflow.Verification{Approved: false, Reason: "needs revision"}, nil
	}
	return workflow.Verification{Approved: true, Reason: "looks good"}, nil
}

func TestWorkflowEngineUsesVerifierGateAndSupportsRework(t *testing.T) {
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	parent, err := k.Spawn(kernel.ProcessSpec{Role: "supervisor"})
	if err != nil {
		t.Fatalf("spawn parent: %v", err)
	}

	engine := workflow.Engine{
		Kernel:    k,
		Artifacts: store,
		Planner: fakePlanner{plan: workflow.Plan{
			Goal: "build a tiny doc bundle",
			Contracts: []workflow.TaskContract{
				{ID: "task-1", Objective: "write intro", ExpectedArtifacts: []string{"worker_output"}},
				{ID: "task-2", Objective: "write appendix", ExpectedArtifacts: []string{"worker_output"}},
			},
		}},
		Worker:            fakeWorker{},
		Verifier:          &fakeVerifier{},
		MaxWorkerAttempts: 2,
	}

	summary, err := engine.Run(context.Background(), parent.PID, "build a tiny doc bundle")
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	if summary.PlanArtifactID == "" {
		t.Fatal("expected plan artifact id")
	}
	if len(summary.Results) != 2 {
		t.Fatalf("expected 2 task results, got %d", len(summary.Results))
	}
	if summary.Results[0].Policy != workflow.VerificationRequired {
		t.Fatalf("expected configured verifier to imply required policy, got %q", summary.Results[0].Policy)
	}
	if !summary.Results[0].Accepted || !summary.Results[0].Approved || summary.Results[0].Attempts != 2 {
		t.Fatalf("expected first task to require rework and then pass, got %#v", summary.Results[0])
	}
	if !summary.Results[1].Approved || summary.Results[1].Attempts != 1 {
		t.Fatalf("expected second task to pass first try, got %#v", summary.Results[1])
	}

	var verification workflow.Verification
	if err := store.ReadJSON(summary.Results[0].VerificationArtifact, &verification); err != nil {
		t.Fatalf("read verification artifact: %v", err)
	}
	if !verification.Approved {
		t.Fatalf("expected final verification approval, got %#v", verification)
	}

	processes, err := k.Processes()
	if err != nil {
		t.Fatalf("list processes: %v", err)
	}
	if len(processes) < 5 {
		t.Fatalf("expected planner, worker, and verifier processes, got %d", len(processes))
	}
}

type rejectingVerifier struct {
	calls int
}

func (r *rejectingVerifier) Verify(_ context.Context, _ workflow.TaskContract, _ workflow.WorkerOutput) (workflow.Verification, error) {
	r.calls++
	return workflow.Verification{Approved: false, Reason: "never good enough"}, nil
}

type failingVerifier struct{}

func (failingVerifier) Verify(_ context.Context, _ workflow.TaskContract, _ workflow.WorkerOutput) (workflow.Verification, error) {
	return workflow.Verification{}, errors.New("verifier backend down")
}

func newTestEngine(t *testing.T) (*kernel.Kernel, *artifact.Store, string, workflow.Engine) {
	t.Helper()
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	parent, err := k.Spawn(kernel.ProcessSpec{Role: "supervisor"})
	if err != nil {
		t.Fatalf("spawn parent: %v", err)
	}
	engine := workflow.Engine{
		Kernel:    k,
		Artifacts: store,
		Planner: fakePlanner{plan: workflow.Plan{
			Goal:      "write a note",
			Contracts: []workflow.TaskContract{{ID: "task-1", Objective: "write note"}},
		}},
		Worker:            fakeWorker{},
		MaxWorkerAttempts: 3,
	}
	return k, store, parent.PID, engine
}

func countRole(t *testing.T, k *kernel.Kernel, role string) int {
	t.Helper()
	processes, err := k.Processes()
	if err != nil {
		t.Fatalf("list processes: %v", err)
	}
	count := 0
	for _, process := range processes {
		if process.Role == role {
			count++
		}
	}
	return count
}

func countArtifacts(t *testing.T, store *artifact.Store, kind string) int {
	t.Helper()
	items, err := store.List()
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	count := 0
	for _, item := range items {
		if item.Type == kind {
			count++
		}
	}
	return count
}

func TestWorkflowEngineWithoutVerifierAcceptsFirstOutput(t *testing.T) {
	k, store, parentPID, engine := newTestEngine(t)

	summary, err := engine.Run(context.Background(), parentPID, "write a note")
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	result := summary.Results[0]
	if result.Policy != workflow.VerificationNone || !result.Accepted || result.Approved {
		t.Fatalf("expected unverified acceptance, got %#v", result)
	}
	if result.Attempts != 1 || result.VerifierPID != "" || result.VerificationArtifact != "" {
		t.Fatalf("expected single attempt without verifier, got %#v", result)
	}
	if got := countRole(t, k, "verifier"); got != 0 {
		t.Fatalf("expected no verifier processes, got %d", got)
	}
	if got := countArtifacts(t, store, "verification"); got != 0 {
		t.Fatalf("expected no verification artifacts, got %d", got)
	}
}

func TestWorkflowEngineRequiredPolicyReportsRejection(t *testing.T) {
	_, _, parentPID, engine := newTestEngine(t)
	verifier := &rejectingVerifier{}
	engine.Verifier = verifier
	engine.Verification = workflow.VerificationRequired

	summary, err := engine.Run(context.Background(), parentPID, "write a note")
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	result := summary.Results[0]
	if result.Accepted || result.Approved || result.Attempts != 3 || verifier.calls != 3 {
		t.Fatalf("expected rejected task after all attempts, got %#v (calls=%d)", result, verifier.calls)
	}
}

func TestWorkflowEngineBestEffortAcceptsRejectedOutput(t *testing.T) {
	k, store, parentPID, engine := newTestEngine(t)
	engine.Verifier = &rejectingVerifier{}
	engine.Verification = workflow.VerificationBestEffort

	summary, err := engine.Run(context.Background(), parentPID, "write a note")
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	result := summary.Results[0]
	if !result.Accepted || result.Approved || result.Attempts != 3 {
		t.Fatalf("expected best-effort acceptance after retries, got %#v", result)
	}
	if result.FinalVerification.Reason != "never good enough" {
		t.Fatalf("expected verifier reason to be kept, got %q", result.FinalVerification.Reason)
	}
	if got := countRole(t, k, "verifier"); got != 3 {
		t.Fatalf("expected 3 verifier processes, got %d", got)
	}
	if got := countArtifacts(t, store, "verification"); got != 3 {
		t.Fatalf("expected 3 verification artifacts, got %d", got)
	}
}

func TestWorkflowEngineVerifierErrors(t *testing.T) {
	_, _, parentPID, engine := newTestEngine(t)
	engine.Verifier = failingVerifier{}

	engine.Verification = workflow.VerificationRequired
	if _, err := engine.Run(context.Background(), parentPID, "write a note"); err == nil {
		t.Fatal("expected required policy to surface verifier error")
	}

	engine.Verification = workflow.VerificationBestEffort
	summary, err := engine.Run(context.Background(), parentPID, "write a note")
	if err != nil {
		t.Fatalf("expected best-effort policy to tolerate verifier error, got %v", err)
	}
	result := summary.Results[0]
	if !result.Accepted || result.Approved || result.Attempts != 1 || result.VerificationArtifact != "" {
		t.Fatalf("expected accepted unverified result, got %#v", result)
	}
}

func TestWorkflowEngineRejectsInconsistentPolicy(t *testing.T) {
	cases := []struct {
		name     string
		policy   workflow.VerificationPolicy
		verifier workflow.Verifier
	}{
		{"required without verifier", workflow.VerificationRequired, nil},
		{"best effort without verifier", workflow.VerificationBestEffort, nil},
		{"none with verifier", workflow.VerificationNone, &rejectingVerifier{}},
		{"unknown policy", workflow.VerificationPolicy("sometimes"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, parentPID, engine := newTestEngine(t)
			engine.Verification = tc.policy
			engine.Verifier = tc.verifier
			_, err := engine.Run(context.Background(), parentPID, "write a note")
			if !errors.Is(err, workflow.ErrInvalidVerificationPolicy) {
				t.Fatalf("expected invalid policy error, got %v", err)
			}
		})
	}
}

func TestParseVerificationPolicy(t *testing.T) {
	cases := map[string]workflow.VerificationPolicy{
		"":            "",
		"none":        workflow.VerificationNone,
		" Required ":  workflow.VerificationRequired,
		"best-effort": workflow.VerificationBestEffort,
		"best_effort": workflow.VerificationBestEffort,
	}
	for input, want := range cases {
		got, err := workflow.ParseVerificationPolicy(input)
		if err != nil || got != want {
			t.Fatalf("parse %q: got %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := workflow.ParseVerificationPolicy("always"); !errors.Is(err, workflow.ErrInvalidVerificationPolicy) {
		t.Fatalf("expected invalid policy error, got %v", err)
	}
}
