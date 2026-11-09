package agentflow_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
)

func toolAction(name string, args map[string]string) map[string]any {
	return map[string]any{"type": "tool", "tool_call": map[string]any{"name": name, "args": args}}
}

// boundWorkflow is a lead that can write the work itself, a writer, and a
// verifier that reads it, all over work, under policy.
func boundWorkflow(work, policy string) agentflow.WorkflowSpec {
	mount := []capability.Mount{{Name: "work", HostPath: work}}
	return agentflow.WorkflowSpec{
		Name:       "bound",
		EntryAgent: "lead",
		Agents: []agentflow.AgentSpec{
			{ID: "lead", SystemPrompt: "Lead.", AllowedAgents: []string{"writer", "verifier"}, Mounts: mount,
				Capabilities: capability.Set{Tools: []string{"spawn_agent", "write_file", "read_file"}, WritePaths: []string{work}, ReadPaths: []string{work}}},
			{ID: "writer", SystemPrompt: "Write.", Mounts: mount, Capabilities: capability.Set{Tools: []string{"write_file"}, WritePaths: []string{work}}},
			{ID: "verifier", SystemPrompt: "Verify.", Mounts: []capability.Mount{{Name: "work", HostPath: work, ReadOnly: true}},
				Capabilities: capability.Set{Tools: []string{"read_file"}, ReadPaths: []string{work}}},
		},
		Verification: &agentflow.VerificationSpec{Policy: policy, Verifier: "verifier"},
	}
}

func runBound(t *testing.T, spec agentflow.WorkflowSpec, legacy bool, actions ...map[string]any) (exitFixture, agentflow.RunSummary) {
	t.Helper()
	f := newExitFixture(t, &sequenceCompleter{actions: actions})
	f.engine.NoVerdictRules = legacy
	summary, err := f.engine.Run(context.Background(), spec, agentflow.RunRequest{Goal: "go"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return f, summary
}

// The entry agent changes the work after a pass: the hash at the root's
// exit is not the hash at the verifier's, so the pass is stale. Before the
// verdict rules the gate looked only at who ran, and passed it.
func TestAPassIsBoundToTheWork(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		work := t.TempDir()
		out := filepath.Join(work, "out.txt")
		f, summary := runBound(t, boundWorkflow(work, "required"), legacy,
			delegateAction("writer"), toolAction("write_file", map[string]string{"path": out, "content": "verified"}),
			delegateAction("verifier"), exitWith(0, "PASS", false),
			toolAction("write_file", map[string]string{"path": out, "content": "changed after the pass"}), exitWith(0, "done", false))
		verdict := summary.Verification
		if legacy {
			if verdict.Status != agentflow.VerdictPass || verdict.WorkAtVerify != nil || summary.ExitStatus.Code != 0 {
				t.Fatalf("legacy: %+v %+v", verdict, summary.ExitStatus)
			}
			continue
		}
		if verdict.Status != agentflow.VerdictStale || !strings.Contains(verdict.Reason, "the work changed after verifier") {
			t.Fatalf("verdict %+v", verdict)
		}
		if verdict.WorkAtVerify == nil || verdict.WorkAtGate == nil || verdict.WorkAtVerify.SHA256 == verdict.WorkAtGate.SHA256 || verdict.WorkAtVerify.Files != 1 {
			t.Fatalf("hashes %+v %+v", verdict.WorkAtVerify, verdict.WorkAtGate)
		}
		if summary.ExitStatus.Code != 1 || !summary.ExitStatus.Retryable {
			t.Fatalf("exit %+v", summary.ExitStatus)
		}
		events, _ := f.kernel.Events()
		hashed := 0
		for _, event := range events {
			if event.Type == agentflow.WorkHashEvent && event.ProcessID == verdict.VerifierPID {
				hashed++
			}
		}
		if hashed != 1 {
			t.Fatalf("%d work hashes on the verifier", hashed)
		}
	}
}

// The work unchanged, a pass stands, and its two hashes match.
func TestAPassOverUnchangedWorkStands(t *testing.T) {
	work := t.TempDir()
	_, summary := runBound(t, boundWorkflow(work, "required"), false,
		delegateAction("writer"), toolAction("write_file", map[string]string{"path": filepath.Join(work, "out.txt"), "content": "v1"}),
		delegateAction("verifier"), exitWith(0, "PASS", false), exitWith(0, "done", false))
	if v := summary.Verification; v.Status != agentflow.VerdictPass || v.WorkAtVerify.SHA256 != v.WorkAtGate.SHA256 || summary.ExitStatus.Code != 0 {
		t.Fatalf("%+v %+v", v, summary.ExitStatus)
	}
}

// A verifier's failure stands while the work is as it judged it, even when
// only the entry agent, which the agent rule leaves out, changes it.
func TestAFailStandsOnlyWhileTheWorkIsAsJudged(t *testing.T) {
	work := t.TempDir()
	out := filepath.Join(work, "out.txt")
	_, summary := runBound(t, boundWorkflow(work, "required"), false,
		delegateAction("writer"), toolAction("write_file", map[string]string{"path": out, "content": "wrong"}),
		delegateAction("verifier"), exitWith(1, "FAIL", false),
		toolAction("write_file", map[string]string{"path": out, "content": "fixed by the lead"}),
		delegateAction("verifier"), exitWith(0, "PASS", false), exitWith(0, "done", false))
	if v := summary.Verification; v.Status != agentflow.VerdictPass || summary.ExitStatus.Code != 0 {
		t.Fatalf("a fail of other work stood: %+v %+v", v, summary.ExitStatus)
	}
}

// gateCheckWorkflow adds a trusted directory holding check.sh, which the
// gate runs after the verifier passes.
func gateCheckWorkflow(t *testing.T, script string) (agentflow.WorkflowSpec, string) {
	t.Helper()
	work, trusted := t.TempDir(), t.TempDir()
	check := filepath.Join(trusted, "check.sh")
	if err := os.WriteFile(check, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := boundWorkflow(work, "required")
	for _, i := range []int{0, 2} {
		agent := &spec.Agents[i]
		agent.Mounts = append(agent.Mounts, capability.Mount{Name: "trusted", HostPath: trusted, ReadOnly: true})
		agent.Capabilities.ReadPaths = append(agent.Capabilities.ReadPaths, trusted)
	}
	spec.Verification.Trusted = []string{trusted}
	spec.Verification.Checks = []agentflow.CheckSpec{{Name: "render", Command: "sh " + check + " " + work, Uses: []string{check}}}
	return spec, check
}

func passingRun() []map[string]any {
	return []map[string]any{delegateAction("verifier"), exitWith(0, "PASS", false), exitWith(0, "done", false)}
}

// The gate's own check decides over the verifier's exit 0: one that fails,
// or cannot run, fails the gate, and the verdict records what ran.
func TestGateChecksDecideOverTheVerifier(t *testing.T) {
	spec, check := gateCheckWorkflow(t, "echo wrong image; exit 1\n")
	_, summary := runBound(t, spec, false, passingRun()...)
	v := summary.Verification
	if v.Status != agentflow.VerdictFail || !strings.Contains(v.Reason, `gate check "render" exited 1, want 0`) || summary.ExitStatus.Code != 1 || summary.ExitStatus.Retryable {
		t.Fatalf("%+v %+v", v, summary.ExitStatus)
	}
	result := v.Checks[0]
	if result.ExitCode != 1 || result.OutputSHA256 == "" || len(result.Uses[check]) != 64 || result.Passed || !strings.HasPrefix(result.Confinement, "sandbox") {
		t.Fatalf("check %+v", result)
	}

	spec, _ = gateCheckWorkflow(t, "exit 0\n")
	if _, summary = runBound(t, spec, false, passingRun()...); summary.Verification.Status != agentflow.VerdictPass || !summary.Verification.Checks[0].Passed {
		t.Fatalf("a passing check: %+v", summary.Verification)
	}

	spec, check = gateCheckWorkflow(t, "exit 0\n")
	_ = os.Remove(check)
	if _, summary = runBound(t, spec, false, passingRun()...); summary.Verification.Status != agentflow.VerdictFail || summary.Verification.Checks[0].Uses[check] != "missing" {
		t.Fatalf("a missing script: %+v", summary.Verification)
	}

	spec, _ = gateCheckWorkflow(t, "exit 1\n")
	if _, summary = runBound(t, spec, true, passingRun()...); summary.Verification.Status != agentflow.VerdictPass || summary.Verification.Checks != nil {
		t.Fatalf("legacy runs no checks: %+v", summary.Verification)
	}
}

// The verifier's task is the spec's; the delegator's goal reaches it marked
// as the delegator's words. Before the verdict rules the goal was the task.
func TestTheVerifierTaskComesFromTheSpec(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		spec := boundWorkflow(t.TempDir(), "required")
		spec.Verification.Task = "Compare out.txt with the reference, line by line."
		f, summary := runBound(t, spec, legacy, passingRun()...)
		process, err := f.kernel.Get(summary.Verification.VerifierPID)
		if err != nil {
			t.Fatal(err)
		}
		goal := process.Metadata["goal"]
		if legacy {
			if goal != "do the verifier part" {
				t.Fatalf("legacy goal %q", goal)
			}
			continue
		}
		if !strings.HasPrefix(goal, "Your task, from the workflow spec:\nCompare out.txt with the reference, line by line.") ||
			!strings.Contains(goal, "lead (") || !strings.Contains(goal, "not your task") || !strings.HasSuffix(goal, "\n| do the verifier part") {
			t.Fatalf("goal %q", goal)
		}
	}
	if !strings.Contains(agentflow.DefaultVerifierTask, "Exit 0 only if the work passes") {
		t.Fatal(agentflow.DefaultVerifierTask)
	}
}

// best_effort: a verifier that judges the work wrong fails the workflow.
// Before the verdict rules it exited 0; a missing verdict still does.
func TestBestEffortFailsOnAFailedVerdict(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		_, summary := runBound(t, boundWorkflow(t.TempDir(), "best_effort"), legacy,
			delegateAction("verifier"), exitWith(1, "FAIL", false), exitWith(0, "done", false))
		want := 1
		if legacy {
			want = 0
		}
		if summary.ExitStatus.Code != want || summary.Verification.Status != agentflow.VerdictFail {
			t.Fatalf("legacy %t: %+v", legacy, summary.ExitStatus)
		}
	}
	_, summary := runBound(t, boundWorkflow(t.TempDir(), "best_effort"), false, exitWith(0, "done", false))
	if summary.ExitStatus.Code != 0 || summary.Verification.Status != agentflow.VerdictMissing {
		t.Fatalf("missing: %+v", summary.ExitStatus)
	}
}

// A failure that stands is not retryable, whatever the verifier claimed.
func TestAStandingFailureIsNotRetryable(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		_, summary := runBound(t, boundWorkflow(t.TempDir(), "required"), legacy,
			delegateAction("verifier"), exitWith(1, "FAIL, retry me", true), exitWith(0, "done", false))
		if summary.ExitStatus.Code != 1 || summary.ExitStatus.Retryable != legacy {
			t.Fatalf("legacy %t: %+v", legacy, summary.ExitStatus)
		}
	}
}

// Spec contract v2's verification rules, each with its error; v1 accepted
// each of these specs.
func TestTheVerificationContract(t *testing.T) {
	work := "/work"
	base := func(edit func(*agentflow.WorkflowSpec)) agentflow.WorkflowSpec {
		spec := boundWorkflow(work, "required")
		edit(&spec)
		return spec
	}
	cases := []struct {
		name string
		spec agentflow.WorkflowSpec
		want string
	}{
		{"unconfined under a required gate", base(func(s *agentflow.WorkflowSpec) {
			s.Agents[1].Capabilities.Tools = []string{"run"}
			s.Agents[1].Capabilities.UnconfinedRun = true
			s.Agents[0].Delegable = capability.Set{Tools: []string{"run"}, UnconfinedRun: true}
		}), "agents[1] (writer): unconfined_run under a required gate"},
		{"a work scope of /", base(func(s *agentflow.WorkflowSpec) {
			s.Agents[1].Mounts = []capability.Mount{{Name: "root", HostPath: "/"}}
			s.Agents[1].Capabilities.WritePaths = []string{"/"}
			s.Agents[0].Mounts = append(s.Agents[0].Mounts, capability.Mount{Name: "root", HostPath: "/"})
			s.Agents[0].Capabilities.WritePaths = []string{"/"}
		}), "agents[1] (writer): writes / under a required gate"},
		{"a trusted path a worker writes", base(func(s *agentflow.WorkflowSpec) {
			s.Verification.Trusted = []string{work + "/baseline"}
		}), "verification.trusted[0] /work/baseline can be written by lead, writer"},
		{"a running verifier that trusts nothing", base(func(s *agentflow.WorkflowSpec) {
			s.Agents[0].Delegable = capability.Set{Tools: []string{"run"}, WritePaths: []string{"/verification"}}
			s.Agents[2].Capabilities.Tools = []string{"read_file", "run"}
			s.Agents[2].Capabilities.WritePaths = []string{"/verification"}
		}), `verifier "verifier" holds run under a required gate and names no verification.trusted paths`},
		{"a check outside the trusted paths", base(func(s *agentflow.WorkflowSpec) {
			s.Verification.Trusted = []string{"/reference"}
			s.Verification.Checks = []agentflow.CheckSpec{{Name: "c", Command: "sh /work/test.sh", Uses: []string{"/work/test.sh"}}}
		}), "verification.checks[0].uses[0] /work/test.sh is not inside a verification.trusted path"},
		{"a check that names nothing it uses", base(func(s *agentflow.WorkflowSpec) {
			s.Verification.Trusted = []string{"/reference"}
			s.Verification.Checks = []agentflow.CheckSpec{{Name: "c", Command: "true"}}
		}), "verification.checks[0]: needs uses"},
		{"a trusted instrument with nothing trusted", base(func(s *agentflow.WorkflowSpec) {
			s.Agents[1].Capabilities.Tools = append(s.Agents[1].Capabilities.Tools, "publish_artifact", "read_file")
			s.Agents[0].Delegable = capability.Set{Tools: []string{"publish_artifact"}}
			s.Verification.Evidence = []agentflow.EvidenceSpec{{Publisher: "writer", Type: "raw", TrustedInstrument: true}}
		}), "verification.evidence[0]: trusted_instrument needs verification.trusted paths"},
		{"a task without a policy", base(func(s *agentflow.WorkflowSpec) {
			s.Verification.Policy = "none"
			s.Verification.Task = "Check."
		}), "verification task, trusted and checks need a verification policy other than none"},
	}
	for _, c := range cases {
		err := c.spec.Validate()
		if !errors.Is(err, agentflow.ErrWorkflowSpecInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s:\n got %v\nwant %q", c.name, err, c.want)
		}
		if err := c.spec.ValidateContractV1(); err != nil {
			t.Errorf("%s: v1 refused it: %v", c.name, err)
		}
	}
	trusted := base(func(s *agentflow.WorkflowSpec) {
		s.Agents[0].Delegable = capability.Set{Tools: []string{"run"}, WritePaths: []string{"/verification"}}
		s.Agents[2].Capabilities.Tools = []string{"read_file", "run"}
		s.Agents[2].Capabilities.WritePaths = []string{"/verification"}
		s.Verification.Trusted = []string{"/reference"}
		s.Verification.Checks = []agentflow.CheckSpec{{Name: "c", Command: "sh /reference/check.sh", Uses: []string{"/reference/check.sh"}}}
	})
	if err := trusted.Validate(); err != nil {
		t.Fatalf("a trusted reference and check: %v", err)
	}
}

// An agent whose write scope the run request derives can write a trusted
// path only as the request says; the run refuses it then.
func TestTrustedPathsAreCheckedAgainstTheRunRequest(t *testing.T) {
	out := t.TempDir()
	spec := gatedSpec("required")
	spec.Verification.Trusted = []string{out}
	f := newExitFixture(t, &sequenceCompleter{actions: passingRun()})
	_, err := f.engine.Run(context.Background(), spec, agentflow.RunRequest{Goal: "go", OutputPath: filepath.Join(out, "out.html")})
	if !errors.Is(err, agentflow.ErrWorkflowSpecInvalid) || !strings.Contains(err.Error(), "with the scopes this run request derives") || !strings.Contains(err.Error(), "writer") {
		t.Fatalf("err %v", err)
	}
}

// benchWorkflow is boundWorkflow with a benchmarker that publishes raw
// evidence, declared trusted_instrument, from bench, and reads trusted.
func benchWorkflow(work, trusted, bench string) agentflow.WorkflowSpec {
	spec := boundWorkflow(work, "required")
	spec.Agents[0].AllowedAgents = append(spec.Agents[0].AllowedAgents, "benchmarker")
	spec.Agents[0].Delegable = capability.Set{Tools: []string{"run", "publish_artifact"}, WritePaths: []string{bench}, ReadPaths: []string{bench, trusted}}
	spec.Agents[0].Mounts = append(spec.Agents[0].Mounts, capability.Mount{Name: "bench", HostPath: bench}, capability.Mount{Name: "trusted", HostPath: trusted, ReadOnly: true})
	spec.Agents = append(spec.Agents, agentflow.AgentSpec{ID: "benchmarker", SystemPrompt: "Bench.",
		Mounts:       []capability.Mount{{Name: "bench", HostPath: bench}, {Name: "work", HostPath: work, ReadOnly: true}, {Name: "trusted", HostPath: trusted, ReadOnly: true}},
		Capabilities: capability.Set{Tools: []string{"run", "read_file", "publish_artifact"}, WritePaths: []string{bench}, ReadPaths: []string{bench, work, trusted}}})
	spec.Verification.Trusted = []string{trusted}
	spec.Verification.Evidence = []agentflow.EvidenceSpec{{Publisher: "benchmarker", Type: "raw", TrustedInstrument: true}}
	return spec
}

// runBench runs benchWorkflow with the benchmarker publishing record after
// command, and returns the evidence check.
func runBench(t *testing.T, spec agentflow.WorkflowSpec, command, record string) (exitFixture, agentflow.RunSummary, agentflow.EvidenceCheck) {
	t.Helper()
	f, summary := runBound(t, spec, false,
		delegateAction("benchmarker"), toolAction("run", map[string]string{"command": command}),
		toolAction("publish_artifact", map[string]string{"type": "raw", "path": record}), exitWith(0, "published", false),
		delegateAction("verifier"), exitWith(1, "not read", false), exitWith(0, "done", false))
	checks, err := f.engine.EvidenceChecks(spec, summary.Verification.VerifierPID)
	if err != nil {
		t.Fatal(err)
	}
	return f, summary, checks[0]
}

// Evidence declared trusted_instrument must come from a run that named only
// trusted files and system tools; a worker-writable script makes it
// untrusted, and the record says what ran.
func TestEvidenceNeedsATrustedInstrument(t *testing.T) {
	for _, fromTrusted := range []bool{true, false} {
		work, trusted, bench := t.TempDir(), t.TempDir(), t.TempDir()
		dir := work
		if fromTrusted {
			dir = trusted
		}
		script := filepath.Join(dir, "bench.sh")
		if err := os.WriteFile(script, []byte("echo frame_ms=1.0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		record := filepath.Join(bench, "raw.txt")
		_, _, check := runBench(t, benchWorkflow(work, trusted, bench), "sh "+script+" > "+record, record)
		if check.ProducedBy == "" || check.ProducedBy == runtime.ProducedByNone || !strings.Contains(check.Command, script) || len(check.Ran[script]) != 64 {
			t.Fatalf("provenance %+v", check)
		}
		switch {
		case fromTrusted && check.Status != agentflow.EvidenceUnread:
			t.Fatalf("a trusted script: %+v", check)
		case !fromTrusted && (check.Status != agentflow.EvidenceUntrusted || len(check.Untrusted) != 1 || check.Untrusted[0] != script):
			t.Fatalf("a worker's script: %+v", check)
		}
	}
}

// A command the instrument rules cannot account for makes its record
// untrusted, and the check says what stopped it. Before the rules, a
// command that named no file was trusted: a replay of such a record judges
// it so again.
func TestEvidenceRefusesACommandItCannotAccountFor(t *testing.T) {
	work, trusted, bench := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "bench.sh"), []byte("echo frame_ms=0.01\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(bench, "raw.txt")
	for _, c := range []struct{ command, refused string }{
		{"cd " + work + " && sh bench.sh > " + record, "cd, which changes the shell"},
		{"echo frame_ms=0.01 > " + record, "an empty account is never trusted"},
	} {
		spec := benchWorkflow(work, trusted, bench)
		f, summary, check := runBench(t, spec, c.command, record)
		if check.Status != agentflow.EvidenceUntrusted || !strings.Contains(check.Refused, c.refused) || len(check.Ran) != 0 {
			t.Fatalf("%q: %+v", c.command, check)
		}
		if evidence := summary.Verification.Evidence; len(evidence) != 1 || evidence[0].Refused != check.Refused {
			t.Fatalf("%q: the verdict's record %+v", c.command, evidence)
		}
		f.engine.NoInstrumentRules = true
		checks, err := f.engine.EvidenceChecks(spec, summary.Verification.VerifierPID)
		if err != nil {
			t.Fatal(err)
		}
		if checks[0].Status != agentflow.EvidenceUnread || checks[0].Refused != "" {
			t.Fatalf("%q before the rules: %+v", c.command, checks[0])
		}
	}
}

// run.started says which verification rules a run ran under, so a replay
// can set them up again.
func TestRunStartedRecordsTheVerificationVersions(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		f := newExitFixture(t, &sequenceCompleter{actions: passingRun()})
		f.engine.NoVerdictRules, f.engine.SpecContractV1, f.engine.NoExitOrigin, f.engine.NoPublishProvenance, f.engine.NoInstrumentRules = legacy, legacy, legacy, legacy, legacy
		summary, err := f.engine.Run(context.Background(), boundWorkflow(t.TempDir(), "required"), agentflow.RunRequest{Goal: "go"})
		if err != nil {
			t.Fatal(err)
		}
		events, _ := f.kernel.Events()
		var data map[string]string
		for _, event := range events {
			if event.Type == agentflow.RunStartedEvent {
				data = event.Data
			}
		}
		want := map[string]string{agentflow.SpecContractField: agentflow.SpecContractV2, agentflow.VerdictRulesField: agentflow.VerdictRulesV1,
			agentflow.ExitOriginField: agentflow.ExitOriginV1, agentflow.PublishProvenanceField: agentflow.PublishProvenanceV1, agentflow.InstrumentRulesField: agentflow.InstrumentRulesV1}
		if legacy {
			want = map[string]string{agentflow.SpecContractField: agentflow.SpecContractV1, agentflow.VerdictRulesField: "", agentflow.ExitOriginField: "", agentflow.PublishProvenanceField: "", agentflow.InstrumentRulesField: ""}
		}
		for field, value := range want {
			if data[field] != value {
				t.Fatalf("legacy %t: %s = %q, want %q", legacy, field, data[field], value)
			}
		}
		root, _ := f.kernel.Await(summary.RootPID)
		if wantOrigin := map[bool]string{false: kernel.ExitOriginEngine, true: ""}[legacy]; root.Origin != wantOrigin {
			t.Fatalf("root origin %q", root.Origin)
		}
	}
}
