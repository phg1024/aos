package workflow

import (
	"context"
	"fmt"
	"strconv"

	"aos/internal/artifact"
	"aos/internal/kernel"
)

type TaskContract struct {
	ID                string   `json:"id"`
	Objective         string   `json:"objective"`
	WriteScope        []string `json:"write_scope,omitempty"`
	ExpectedArtifacts []string `json:"expected_artifacts,omitempty"`
	VerificationSteps []string `json:"verification_steps,omitempty"`
}

type Plan struct {
	Goal      string         `json:"goal"`
	Contracts []TaskContract `json:"contracts"`
}

type WorkerOutput struct {
	Summary     string            `json:"summary"`
	ArtifactIDs []string          `json:"artifact_ids,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type Verification struct {
	Approved bool              `json:"approved"`
	Reason   string            `json:"reason"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type Planner interface {
	Plan(ctx context.Context, goal string) (Plan, error)
}

type Worker interface {
	Execute(ctx context.Context, contract TaskContract, attempt int) (WorkerOutput, error)
}

type Verifier interface {
	Verify(ctx context.Context, contract TaskContract, output WorkerOutput) (Verification, error)
}

type TaskResult struct {
	ContractID           string       `json:"contract_id"`
	Attempts             int          `json:"attempts"`
	Approved             bool         `json:"approved"`
	WorkerPID            string       `json:"worker_pid"`
	VerifierPID          string       `json:"verifier_pid"`
	WorkerOutputArtifact string       `json:"worker_output_artifact"`
	VerificationArtifact string       `json:"verification_artifact"`
	FinalVerification    Verification `json:"final_verification"`
}

type Summary struct {
	PlanArtifactID string       `json:"plan_artifact_id"`
	Results        []TaskResult `json:"results"`
}

type Engine struct {
	Kernel            *kernel.Kernel
	Artifacts         *artifact.Store
	Planner           Planner
	Worker            Worker
	Verifier          Verifier
	MaxWorkerAttempts int
}

func (e *Engine) Run(ctx context.Context, parentPID, goal string) (Summary, error) {
	attemptLimit := e.MaxWorkerAttempts
	if attemptLimit <= 0 {
		attemptLimit = 1
	}

	plannerProcess, err := e.Kernel.Spawn(kernel.ProcessSpec{
		ParentPID: parentPID,
		Role:      "planner",
		Metadata:  map[string]string{"goal": goal},
	})
	if err != nil {
		return Summary{}, err
	}
	plan, err := e.Planner.Plan(ctx, goal)
	if err != nil {
		_, _ = e.Kernel.Exit(plannerProcess.PID, kernel.ExitStatus{Code: 1, Reason: err.Error(), Retryable: false})
		return Summary{}, err
	}
	planArtifact, err := e.Artifacts.CreateJSON("task_plan", plannerProcess.PID, plan, nil, map[string]string{
		"goal": goal,
	})
	if err != nil {
		return Summary{}, err
	}
	if _, err := e.Kernel.Exit(plannerProcess.PID, kernel.ExitStatus{Code: 0, Reason: "planned", Retryable: false}); err != nil {
		return Summary{}, err
	}

	summary := Summary{PlanArtifactID: planArtifact.ID}
	for _, contract := range plan.Contracts {
		result, err := e.executeContract(ctx, parentPID, contract, attemptLimit)
		if err != nil {
			return Summary{}, err
		}
		summary.Results = append(summary.Results, result)
	}

	return summary, nil
}

func (e *Engine) executeContract(ctx context.Context, parentPID string, contract TaskContract, attemptLimit int) (TaskResult, error) {
	var final TaskResult
	for attempt := 1; attempt <= attemptLimit; attempt++ {
		workerProcess, err := e.Kernel.Spawn(kernel.ProcessSpec{
			ParentPID: parentPID,
			Role:      "worker",
			Metadata: map[string]string{
				"contract_id": contract.ID,
				"attempt":     strconv.Itoa(attempt),
			},
		})
		if err != nil {
			return TaskResult{}, err
		}
		output, err := e.Worker.Execute(ctx, contract, attempt)
		if err != nil {
			_, _ = e.Kernel.Exit(workerProcess.PID, kernel.ExitStatus{Code: 1, Reason: err.Error(), Retryable: attempt < attemptLimit})
			return TaskResult{}, err
		}
		workerArtifact, err := e.Artifacts.CreateJSON("worker_output", workerProcess.PID, output, nil, map[string]string{
			"contract_id": contract.ID,
			"attempt":     strconv.Itoa(attempt),
		})
		if err != nil {
			return TaskResult{}, err
		}
		if _, err := e.Kernel.Exit(workerProcess.PID, kernel.ExitStatus{Code: 0, Reason: "produced", Retryable: false}); err != nil {
			return TaskResult{}, err
		}

		verifierProcess, err := e.Kernel.Spawn(kernel.ProcessSpec{
			ParentPID: parentPID,
			Role:      "verifier",
			Metadata: map[string]string{
				"contract_id": contract.ID,
				"attempt":     strconv.Itoa(attempt),
			},
		})
		if err != nil {
			return TaskResult{}, err
		}
		verification, err := e.Verifier.Verify(ctx, contract, output)
		if err != nil {
			_, _ = e.Kernel.Exit(verifierProcess.PID, kernel.ExitStatus{Code: 1, Reason: err.Error(), Retryable: false})
			return TaskResult{}, err
		}
		verificationArtifact, err := e.Artifacts.CreateJSON("verification", verifierProcess.PID, verification, []string{workerArtifact.ID}, map[string]string{
			"contract_id": contract.ID,
			"attempt":     strconv.Itoa(attempt),
		})
		if err != nil {
			return TaskResult{}, err
		}
		if _, err := e.Kernel.Exit(verifierProcess.PID, kernel.ExitStatus{Code: 0, Reason: fmt.Sprintf("approved=%t", verification.Approved), Retryable: false}); err != nil {
			return TaskResult{}, err
		}

		final = TaskResult{
			ContractID:           contract.ID,
			Attempts:             attempt,
			Approved:             verification.Approved,
			WorkerPID:            workerProcess.PID,
			VerifierPID:          verifierProcess.PID,
			WorkerOutputArtifact: workerArtifact.ID,
			VerificationArtifact: verificationArtifact.ID,
			FinalVerification:    verification,
		}
		if verification.Approved {
			return final, nil
		}
	}
	return final, nil
}
