package workflow_test

import (
	"context"
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
	if !summary.Results[0].Approved || summary.Results[0].Attempts != 2 {
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
