package agentflow_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/capability"
	"aos/internal/kernel"
)

// gatedSpec has a coordinator that delegates to a writer (work-changing)
// and a verifier, gated by the given policy.
func gatedSpec(policy string) agentflow.WorkflowSpec {
	return agentflow.WorkflowSpec{
		Name:       "gated",
		EntryAgent: "coordinator",
		Agents: []agentflow.AgentSpec{
			{ID: "coordinator", SystemPrompt: "Coordinate.", AllowedAgents: []string{"writer", "verifier"},
				Capabilities: capability.Set{Tools: []string{"spawn_agent"}},
				Delegable:    capability.Set{Tools: []string{"write_file", "read_file"}}},
			{ID: "writer", SystemPrompt: "Write.", Capabilities: capability.Set{Tools: []string{"write_file"}}},
			{ID: "verifier", SystemPrompt: "Verify.", Capabilities: capability.Set{Tools: []string{"read_file"}}},
		},
		Verification: &agentflow.VerificationSpec{Policy: policy, Verifier: "verifier"},
	}
}

func runGated(t *testing.T, policy string, actions ...map[string]any) (exitFixture, agentflow.RunSummary) {
	t.Helper()
	f := newExitFixture(t, &sequenceCompleter{actions: actions})
	summary, err := f.engine.Run(context.Background(), gatedSpec(policy), agentflow.RunRequest{Goal: "go"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return f, summary
}

func TestRequiredVerificationPassesAfterWriter(t *testing.T) {
	f, summary := runGated(t, "required",
		delegateAction("writer"), exitWith(0, "wrote", false),
		delegateAction("verifier"), exitWith(0, "PASS", false),
		exitWith(0, "done", false))
	if status := f.rootStatus(t, summary); status != (kernel.ExitStatus{Code: 0, Reason: "workflow completed"}) {
		t.Fatalf("root exit = %+v", status)
	}
	if summary.Verification == nil || summary.Verification.Status != agentflow.VerdictPass {
		t.Fatalf("verdict = %+v", summary.Verification)
	}
}

func TestRequiredVerificationFailsRootDespiteEntrySuccess(t *testing.T) {
	f, summary := runGated(t, "required",
		delegateAction("writer"), exitWith(0, "wrote", false),
		delegateAction("verifier"), exitWith(1, "FAIL: 3 scenes differ", false),
		exitWith(0, "all good", false))
	status := f.rootStatus(t, summary)
	verifierPID := summary.Verification.VerifierPID
	want := kernel.ExitStatus{Code: 1, Reason: "verification required: verifier " + verifierPID + " failed: FAIL: 3 scenes differ"}
	if status != want {
		t.Fatalf("root exit = %+v, want %+v", status, want)
	}
}

func TestRequiredVerificationWithoutVerifierRunFails(t *testing.T) {
	f, summary := runGated(t, "required",
		delegateAction("writer"), exitWith(0, "wrote", false),
		exitWith(0, "done", false))
	if status := f.rootStatus(t, summary); status != (kernel.ExitStatus{Code: 1, Reason: "verification required: no verifier run"}) {
		t.Fatalf("root exit = %+v", status)
	}
	if summary.Verification.Status != agentflow.VerdictMissing || summary.Verification.VerifierExit != nil {
		t.Fatalf("verdict = %+v", summary.Verification)
	}
}

func TestRequiredVerificationIsStaleAfterWriterRunsAgain(t *testing.T) {
	f, summary := runGated(t, "required",
		delegateAction("writer"), exitWith(0, "wrote", false),
		delegateAction("verifier"), exitWith(0, "PASS", false),
		delegateAction("writer"), exitWith(0, "changed it again", false),
		exitWith(0, "done", false))
	status := f.rootStatus(t, summary)
	verdict := summary.Verification
	if verdict.Status != agentflow.VerdictStale || verdict.StaleAgent != "writer" || verdict.StalePID == "" {
		t.Fatalf("verdict = %+v", verdict)
	}
	want := "verification required: verdict stale; writer (" + verdict.StalePID + ") ran after verifier " + verdict.VerifierPID
	if status.Code != 1 || status.Reason != want || !status.Retryable {
		t.Fatalf("root exit = %+v, want reason %q", status, want)
	}
}

func TestRequiredVerificationUsesLatestVerdict(t *testing.T) {
	f, summary := runGated(t, "required",
		delegateAction("writer"), exitWith(0, "wrote", false),
		delegateAction("verifier"), exitWith(1, "FAIL", false),
		delegateAction("writer"), exitWith(0, "fixed", false),
		delegateAction("verifier"), exitWith(0, "PASS", false),
		exitWith(0, "done", false))
	if status := f.rootStatus(t, summary); status.Code != 0 {
		t.Fatalf("a fresh pass after a fix should succeed, got %+v", status)
	}
}

func TestEntryFailureTakesPrecedenceOverVerdict(t *testing.T) {
	f, summary := runGated(t, "required",
		delegateAction("verifier"), exitWith(0, "PASS", false),
		exitWith(2, "gave up", false))
	if status := f.rootStatus(t, summary); status.Code != 2 || !strings.HasPrefix(status.Reason, "entry agent coordinator exited with code 2") {
		t.Fatalf("root exit = %+v", status)
	}
}

func TestBestEffortVerificationRecordsFailureWithoutGating(t *testing.T) {
	f, summary := runGated(t, "best_effort",
		delegateAction("writer"), exitWith(0, "wrote", false),
		delegateAction("verifier"), exitWith(1, "FAIL", false),
		exitWith(0, "done", false))
	if status := f.rootStatus(t, summary); status.Code != 0 {
		t.Fatalf("best_effort must not gate, got %+v", status)
	}
	if summary.Verification == nil || summary.Verification.Status != agentflow.VerdictFail {
		t.Fatalf("best_effort should still record the verdict, got %+v", summary.Verification)
	}
}

// The verdict artifact is the root's, and it matches the kernel's record of
// the verifier's exit rather than anything an agent wrote.
func TestVerificationArtifactIsProducedByRoot(t *testing.T) {
	f, summary := runGated(t, "required",
		delegateAction("writer"), exitWith(0, "wrote", false),
		delegateAction("verifier"), exitWith(1, "FAIL", true),
		exitWith(0, "done", false))
	record, err := f.store.Metadata(summary.VerificationID)
	if err != nil {
		t.Fatalf("verification artifact: %v", err)
	}
	if record.Type != "verification" || record.ProducerPID != summary.RootPID {
		t.Fatalf("artifact %+v should be a verification produced by root %s", record, summary.RootPID)
	}
	var verdict agentflow.Verdict
	if err := f.store.ReadJSON(record.ID, &verdict); err != nil {
		t.Fatalf("read verdict: %v", err)
	}
	recorded, err := f.kernel.Await(verdict.VerifierPID)
	if err != nil {
		t.Fatalf("await verifier: %v", err)
	}
	if verdict.VerifierExit == nil || *verdict.VerifierExit != recorded || recorded != (kernel.ExitStatus{Code: 1, Reason: "FAIL", Retryable: true}) {
		t.Fatalf("verdict exit %+v should match the kernel record %+v", verdict.VerifierExit, recorded)
	}
}

func TestNoVerificationBlockKeepsBehaviour(t *testing.T) {
	spec := gatedSpec("required")
	spec.Verification = nil
	f := newExitFixture(t, &sequenceCompleter{actions: []map[string]any{delegateAction("writer"), exitWith(0, "wrote", false), exitWith(0, "done", false)}})
	summary, err := f.engine.Run(context.Background(), spec, agentflow.RunRequest{Goal: "go"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.rootStatus(t, summary).Code != 0 || summary.Verification != nil || summary.VerificationID != "" {
		t.Fatalf("no verification block should change nothing: %+v", summary)
	}
}

func TestValidateVerification(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*agentflow.WorkflowSpec)
		want string
	}{
		"missing verifier":   {func(s *agentflow.WorkflowSpec) { s.Verification.Verifier = "" }, "needs a verifier"},
		"unknown verifier":   {func(s *agentflow.WorkflowSpec) { s.Verification.Verifier = "auditor" }, `verifier "auditor" is not an agent`},
		"verifier is entry":  {func(s *agentflow.WorkflowSpec) { s.Verification.Verifier = "coordinator" }, "cannot be the entry agent"},
		"unreachable":        {func(s *agentflow.WorkflowSpec) { s.Agents[0].AllowedAgents = []string{"writer"} }, "not reachable from entry agent"},
		"unknown policy":     {func(s *agentflow.WorkflowSpec) { s.Verification.Policy = "sometimes" }, "invalid verification policy"},
		"best_effort checks": {func(s *agentflow.WorkflowSpec) { s.Verification.Policy = "best_effort"; s.Verification.Verifier = "" }, "needs a verifier"},
	} {
		spec := gatedSpec("required")
		tc.edit(&spec)
		err := spec.Validate()
		if !errors.Is(err, agentflow.ErrWorkflowSpecInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: got %v, want an invalid-spec error mentioning %q", name, err, tc.want)
		}
	}

	reachedThroughWriter := gatedSpec("required")
	reachedThroughWriter.Agents[0].AllowedAgents = []string{"writer"}
	reachedThroughWriter.Agents[1].AllowedAgents = []string{"verifier"}
	reachedThroughWriter.Agents[1].Delegable = capability.Set{Tools: []string{"read_file"}}
	if err := reachedThroughWriter.Validate(); err != nil {
		t.Fatalf("a verifier reachable through another agent is valid: %v", err)
	}
	for _, policy := range []string{"none", ""} {
		spec := gatedSpec(policy)
		spec.Verification.Verifier = ""
		if err := spec.Validate(); err != nil {
			t.Fatalf("policy %q needs no verifier: %v", policy, err)
		}
	}
}
