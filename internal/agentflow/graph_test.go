package agentflow_test

import "testing"

// From the workflow_summary an operator can walk the whole run: every
// agent_run, each run's final checkpoint, and the verdict with the
// verifier's checkpoint.
func TestRunGraphIsWalkableFromTheSummary(t *testing.T) {
	f, summary := runGated(t, "required",
		delegateAction("writer"), exitWith(0, "wrote", false),
		delegateAction("verifier"), exitWith(0, "PASS", false),
		exitWith(0, "done", false))
	top, err := f.store.Metadata(summary.ResultID)
	if err != nil || top.Type != "workflow_summary" || top.ProducerPID != summary.RootPID {
		t.Fatalf("summary artifact %+v %v", top, err)
	}
	agentRuns, verifications := 0, 0
	for _, parentID := range top.ParentIDs {
		parent, err := f.store.Metadata(parentID)
		if err != nil {
			t.Fatalf("summary parent %s: %v", parentID, err)
		}
		if len(parent.ParentIDs) != 1 {
			t.Fatalf("%s %s should name exactly one checkpoint, has %v", parent.Type, parent.ID, parent.ParentIDs)
		}
		checkpoint, err := f.store.Metadata(parent.ParentIDs[0])
		if err != nil || checkpoint.Type != "checkpoint" {
			t.Fatalf("%s %s should point at a checkpoint, got %+v %v", parent.Type, parent.ID, checkpoint, err)
		}
		switch parent.Type {
		case "agent_run":
			agentRuns++
			if checkpoint.ProducerPID != parent.ProducerPID {
				t.Fatalf("agent_run %s should point at its own checkpoint", parent.ID)
			}
		case "verification":
			verifications++
			if checkpoint.ProducerPID != summary.Verification.VerifierPID {
				t.Fatalf("verification should point at the verifier's checkpoint, got producer %s", checkpoint.ProducerPID)
			}
		default:
			t.Fatalf("unexpected summary parent type %s", parent.Type)
		}
	}
	if agentRuns != 3 || verifications != 1 {
		t.Fatalf("summary should reach 3 agent runs and the verdict, got %d and %d", agentRuns, verifications)
	}
}
