package scenario

import (
	"context"
	"path/filepath"
	"strconv"
	"time"

	"aos/internal/artifact"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/workflow"
)

type Report struct {
	Name                    string   `json:"name"`
	ParentPID               string   `json:"parent_pid"`
	ProcessIDs              []string `json:"process_ids"`
	ArtifactIDs             []string `json:"artifact_ids"`
	VerificationArtifactIDs []string `json:"verification_artifact_ids"`
	EventCount              int      `json:"event_count"`
	Notes                   []string `json:"notes,omitempty"`
}

type Runner struct {
	Kernel    *kernel.Kernel
	Artifacts *artifact.Store
	root      string
}

func NewRunner(root string) (*Runner, error) {
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		return nil, err
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		return nil, err
	}
	return &Runner{Kernel: k, Artifacts: store, root: root}, nil
}

func (r *Runner) RunDocsAssembly(ctx context.Context) (Report, error) {
	parent, err := r.Kernel.Spawn(kernel.ProcessSpec{Role: "scenario-docs"})
	if err != nil {
		return Report{}, err
	}
	engine := workflow.Engine{
		Kernel:            r.Kernel,
		Artifacts:         r.Artifacts,
		Planner:           docsPlanner{},
		Worker:            docsWorker{},
		Verifier:          docsVerifier{},
		MaxWorkerAttempts: 1,
	}
	_, err = engine.Run(ctx, parent.PID, "assemble docs")
	if err != nil {
		return Report{}, err
	}
	return r.report("docs-assembly", parent.PID)
}

func (r *Runner) RunParallelRefactor(ctx context.Context) (Report, error) {
	parent, err := r.Kernel.Spawn(kernel.ProcessSpec{Role: "scenario-refactor"})
	if err != nil {
		return Report{}, err
	}
	manager, err := lease.New(filepath.Join(r.root, "leases-refactor"))
	if err != nil {
		return Report{}, err
	}
	owner, _ := r.Kernel.Spawn(kernel.ProcessSpec{ParentPID: parent.PID, Role: "worker-refactor-a"})
	waiter, _ := r.Kernel.Spawn(kernel.ProcessSpec{ParentPID: parent.PID, Role: "worker-refactor-b"})

	if _, err := manager.AcquireForProcess(r.Kernel, "src/shared.go", owner.PID, time.Minute); err != nil {
		return Report{}, err
	}
	if _, err := manager.AcquireForProcess(r.Kernel, "src/shared.go", waiter.PID, time.Minute); err != nil && err != lease.ErrLeaseHeld {
		return Report{}, err
	}
	if err := manager.ReleaseAndWake(r.Kernel, "src/shared.go", owner.PID, waiter.PID); err != nil {
		return Report{}, err
	}
	if _, err := r.Kernel.Exit(owner.PID, kernel.ExitStatus{Code: 0, Reason: "refactor applied"}); err != nil {
		return Report{}, err
	}
	if _, err := r.Kernel.Exit(waiter.PID, kernel.ExitStatus{Code: 0, Reason: "refactor applied after wake"}); err != nil {
		return Report{}, err
	}
	return r.report("parallel-refactor", parent.PID)
}

func (r *Runner) RunDataExtraction(ctx context.Context) (Report, error) {
	parent, err := r.Kernel.Spawn(kernel.ProcessSpec{Role: "scenario-extraction"})
	if err != nil {
		return Report{}, err
	}
	engine := workflow.Engine{
		Kernel:            r.Kernel,
		Artifacts:         r.Artifacts,
		Planner:           extractionPlanner{},
		Worker:            extractionWorker{},
		Verifier:          extractionVerifier{},
		MaxWorkerAttempts: 2,
	}
	_, err = engine.Run(ctx, parent.PID, "extract records")
	if err != nil {
		return Report{}, err
	}
	return r.report("data-extraction", parent.PID)
}

func (r *Runner) report(name, parentPID string) (Report, error) {
	processes, err := r.Kernel.Processes()
	if err != nil {
		return Report{}, err
	}
	artifacts, err := r.Artifacts.List()
	if err != nil {
		return Report{}, err
	}
	events, err := r.Kernel.Events()
	if err != nil {
		return Report{}, err
	}

	report := Report{Name: name, ParentPID: parentPID, EventCount: len(events)}
	for _, process := range processes {
		if process.PID == parentPID || process.ParentPID == parentPID {
			report.ProcessIDs = append(report.ProcessIDs, process.PID)
		}
	}
	for _, item := range artifacts {
		report.ArtifactIDs = append(report.ArtifactIDs, item.ID)
		if item.Type == "verification" {
			report.VerificationArtifactIDs = append(report.VerificationArtifactIDs, item.ID)
		}
	}
	return report, nil
}

func (r *Runner) ArtifactsPath() string {
	return filepath.Join(r.root, "artifacts")
}

type docsPlanner struct{}

func (docsPlanner) Plan(_ context.Context, goal string) (workflow.Plan, error) {
	return workflow.Plan{
		Goal: goal,
		Contracts: []workflow.TaskContract{
			{ID: "docs-core", Objective: "summarize core subsystem"},
			{ID: "docs-api", Objective: "summarize API surface"},
		},
	}, nil
}

type docsWorker struct{}

func (docsWorker) Execute(_ context.Context, contract workflow.TaskContract, attempt int) (workflow.WorkerOutput, error) {
	return workflow.WorkerOutput{
		Summary: "summary " + contract.Objective,
		Metadata: map[string]string{
			"attempt": strconv.Itoa(attempt),
		},
	}, nil
}

type docsVerifier struct{}

func (docsVerifier) Verify(_ context.Context, contract workflow.TaskContract, output workflow.WorkerOutput) (workflow.Verification, error) {
	return workflow.Verification{
		Approved: output.Summary != "",
		Reason:   contract.ID + " reviewed",
	}, nil
}

type extractionPlanner struct{}

func (extractionPlanner) Plan(_ context.Context, goal string) (workflow.Plan, error) {
	return workflow.Plan{
		Goal: goal,
		Contracts: []workflow.TaskContract{
			{ID: "extract-1", Objective: "extract listing 1"},
			{ID: "extract-2", Objective: "extract listing 2"},
		},
	}, nil
}

type extractionWorker struct{}

func (extractionWorker) Execute(_ context.Context, contract workflow.TaskContract, attempt int) (workflow.WorkerOutput, error) {
	return workflow.WorkerOutput{
		Summary: `{"id":"` + contract.ID + `","attempt":` + strconv.Itoa(attempt) + `}`,
	}, nil
}

type extractionVerifier struct{}

func (extractionVerifier) Verify(_ context.Context, contract workflow.TaskContract, output workflow.WorkerOutput) (workflow.Verification, error) {
	return workflow.Verification{
		Approved: output.Summary != "" && contract.ID != "",
		Reason:   "schema looks valid",
	}, nil
}
