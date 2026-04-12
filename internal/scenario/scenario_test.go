package scenario_test

import (
	"context"
	"path/filepath"
	"testing"

	"aos/internal/scenario"
)

func TestScenarioRunnerDocsAssembly(t *testing.T) {
	runner, err := scenario.NewRunner(filepath.Join(t.TempDir(), "docs"))
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	report, err := runner.RunDocsAssembly(context.Background())
	if err != nil {
		t.Fatalf("run docs assembly: %v", err)
	}
	if len(report.ProcessIDs) < 3 {
		t.Fatalf("expected planner/worker/verifier processes, got %d", len(report.ProcessIDs))
	}
	if len(report.VerificationArtifactIDs) == 0 {
		t.Fatal("expected verification artifacts")
	}
}

func TestScenarioRunnerParallelRefactor(t *testing.T) {
	runner, err := scenario.NewRunner(filepath.Join(t.TempDir(), "refactor"))
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	report, err := runner.RunParallelRefactor(context.Background())
	if err != nil {
		t.Fatalf("run refactor: %v", err)
	}
	if len(report.ProcessIDs) < 3 {
		t.Fatalf("expected parent and two workers, got %d", len(report.ProcessIDs))
	}
	if report.EventCount == 0 {
		t.Fatal("expected event trace entries")
	}
}

func TestScenarioRunnerDataExtraction(t *testing.T) {
	runner, err := scenario.NewRunner(filepath.Join(t.TempDir(), "extract"))
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	report, err := runner.RunDataExtraction(context.Background())
	if err != nil {
		t.Fatalf("run data extraction: %v", err)
	}
	if len(report.ArtifactIDs) == 0 {
		t.Fatal("expected extraction artifacts")
	}
	if len(report.VerificationArtifactIDs) == 0 {
		t.Fatal("expected verification artifacts")
	}
}
