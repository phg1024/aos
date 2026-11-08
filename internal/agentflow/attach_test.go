package agentflow_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/kernel"
	"aos/internal/runtime"
)

// replied is what a model wrote, read by the parser every reply goes through.
func replied(format string, handle func() string) step {
	return func(runtime.GenerateRequest) runtime.GenerateResult {
		raw := fmt.Sprintf(format, handle())
		result, ok := agentflow.ParseReply(raw)
		if !ok {
			panic("not parsed: " + raw)
		}
		return result
	}
}

// historyOf is everything an agent's last request held.
func historyOf(s *script) string {
	requests := s.requests()
	if len(requests) == 0 {
		return ""
	}
	var out strings.Builder
	for _, message := range requests[len(requests)-1].History {
		out.WriteString(message.Content + "\n")
	}
	return out.String()
}

// Where a reply put a delegation's artifacts, and which of the parser's paths reads it.
// live4's coordinator wrote the second and third, and nothing was attached or said.
var placements = []struct{ name, reply string }{
	{"inside the delegate", `{"action":{"type":"delegate","delegate":{"agent_id":"verifier","goal":"check the work","artifacts":"%s"}}}`},
	{"at the action level, before the delegate (live4 seq 186)", `{"action":{"type":"delegate","artifacts":"%s","notes":"Attach the raw_record.","delegate":{"agent_id":"verifier","goal":"check the attached raw_record"}}}`},
	{"at the action level, after the delegate (live4 seq 504)", `{"action":{"type":"delegate","notes":"Resume it with the record.","delegate":{"agent_id":"verifier","goal":"check the work"},"artifacts":"%s"}}`},
	{"at the action level, as a list", `{"action":{"type":"delegate","artifacts":["%s"],"delegate":{"agent_id":"verifier","goal":"check the work"}}}`},
	{"in a delegate written flat", `{"action":{"type":"delegate","agent_id":"verifier","goal":"check the work","artifacts":"%s"}}`},
	{"with no action envelope", `{"type":"delegate","delegate":{"agent_id":"verifier","goal":"check the work"},"artifacts":"%s"}`},
	{"after prose", "I attach the record.\n" + `{"action":{"type":"delegate","artifacts":"%s","delegate":{"agent_id":"verifier","goal":"check the work"}}}`},
	{"in loose JSON the parser repairs", `{action:{type:"delegate",artifacts:"%s",delegate:{agent_id:"verifier",goal:"check the work",},},}`},
	{"in spawn_agent's args", `{"action":{"type":"tool","tool_call":{"name":"spawn_agent","args":{"agent_id":"verifier","goal":"check the work","artifacts":"%s"}}}}`},
	{"beside spawn_agent's tool_call", `{"action":{"type":"tool","artifacts":"%s","tool_call":{"name":"spawn_agent","args":{"agent_id":"verifier","goal":"check the work"}}}}`},
	{"beside spawn_agent's args", `{"action":{"type":"tool","tool_call":{"name":"spawn_agent","args":{"agent_id":"verifier","goal":"check the work"},"artifacts":"%s"}}}`},
	{"beside spawn_agent's args, as a list", `{"action":{"type":"tool","tool_call":{"name":"spawn_agent","args":{"agent_id":"verifier","goal":"check the work"},"artifacts":["%s"]}}}`},
}

func TestADelegationsArtifactsCountWhereverTheReplyPutThem(t *testing.T) {
	for _, placement := range placements {
		t.Run(placement.name, func(t *testing.T) {
			f := newEvidenceFixture(t)
			coordinator := &script{replies: []step{delegateStep("benchmarker"), replied(placement.reply, f.latest), finished}}
			verifier := &script{replies: joined(f.wholeRead(), []step{pass()})}
			engine := f.engine(scripts{"coordinator": coordinator, "benchmarker": {replies: []step{publishFile(f), finished}}, "verifier": verifier})
			summary, err := engine.Run(context.Background(), f.spec(rawRecord, 4, nil), agentflow.RunRequest{Goal: "verify"})
			if err != nil {
				t.Fatal(err)
			}
			if summary.Verification.Status != agentflow.VerdictPass {
				t.Fatalf("verdict %+v\ncoordinator:\n%s", summary.Verification, historyOf(coordinator))
			}
			grants := f.grants(t)
			if len(grants) != 1 || grants[0].ProducerPID != verifierPID(f) || grants[0].Metadata["artifacts"] != f.latest() {
				t.Fatalf("grants %+v", grants)
			}
			if first := verifier.requests()[0].History[0].Content; !strings.Contains(first, "Attached to this delegation") || !strings.Contains(first, f.latest()) {
				t.Fatalf("the verifier's first message: %q", first)
			}
			told := fmt.Sprintf("delegate_result agent=verifier pid=%s attached=%s ", verifierPID(f), f.latest())
			if strings.Contains(placement.reply, "spawn_agent") {
				told = "attached:" + f.latest()
			}
			if history := historyOf(coordinator); !strings.Contains(history, told) {
				t.Fatalf("the coordinator was not told %q:\n%s", told, history)
			}
		})
	}
}

// Two places that differ start nothing: the runtime does not pick one.
func TestArtifactsInTwoPlacesThatDifferAreRefused(t *testing.T) {
	f := newEvidenceFixture(t)
	other := func() string { return f.latest() + ",art-999999" }
	coordinator := &script{replies: []step{
		delegateStep("benchmarker"),
		replied(`{"action":{"type":"delegate","artifacts":"`+"%s"+`","delegate":{"agent_id":"verifier","goal":"check the work","artifacts":"art-999999"}}}`, other),
		finished,
	}}
	verifier := &script{replies: []step{pass()}}
	engine := f.engine(scripts{"coordinator": coordinator, "benchmarker": {replies: []step{publishFile(f), finished}}, "verifier": verifier})
	if _, err := engine.Run(context.Background(), f.spec(rawRecord, 4, nil), agentflow.RunRequest{Goal: "verify"}); err != nil {
		t.Fatal(err)
	}
	if len(verifier.requests()) != 0 || len(f.grants(t)) != 0 {
		t.Fatalf("the verifier ran %d steps; grants %+v", len(verifier.requests()), f.grants(t))
	}
	errors := toolErrorsOf(coordinator.requests())
	if !strings.Contains(errors, "tool_error name=delegate error=artifacts not attached: two lists that differ") || !strings.Contains(errors, `give artifacts once, inside \"delegate\"`) && !strings.Contains(errors, `give artifacts once, inside "delegate"`) {
		t.Fatalf("tool errors:\n%s", errors)
	}
}

// A delegation that attaches nothing says so, for a new process and for a resumed one,
// and a resumed one is granted what the resuming delegation attached.
func TestTheDelegatorIsToldWhatWasAttached(t *testing.T) {
	f := newEvidenceFixture(t)
	coordinator := &script{replies: []step{
		delegateStep("benchmarker"),
		delegateStep("verifier"),
		replied(`{"action":{"type":"delegate","notes":"Resume it with the record.","delegate":{"agent_id":"verifier","goal":"here it is"},"artifacts":"%s"}}`, f.latest),
		finished,
	}}
	verifier := &script{replies: joined([]step{waitStep("I need the record.")}, f.wholeRead(), []step{pass()})}
	engine := f.engine(scripts{"coordinator": coordinator, "benchmarker": {replies: []step{publishFile(f), finished}}, "verifier": verifier})
	summary, err := engine.Run(context.Background(), f.spec(rawRecord, 5, nil), agentflow.RunRequest{Goal: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Verification.Status != agentflow.VerdictPass {
		t.Fatalf("verdict %+v", summary.Verification)
	}
	pid := verifierPID(f)
	history := historyOf(coordinator)
	for _, want := range []string{
		"delegate_result agent=benchmarker pid=",
		fmt.Sprintf("delegate_result agent=verifier pid=%s attached=none status=waiting", pid),
		fmt.Sprintf("delegate_result agent=verifier pid=%s attached=%s exit_code=0", pid, f.latest()),
	} {
		if !strings.Contains(history, want) {
			t.Fatalf("lacks %q:\n%s", want, history)
		}
	}
	if !strings.Contains(history, "attached=none exit_code=0") {
		t.Fatalf("the benchmarker's delegation should say attached=none:\n%s", history)
	}
	grants := f.grants(t)
	if len(grants) != 1 || grants[0].ProducerPID != pid || grants[0].Metadata["artifacts"] != f.latest() {
		t.Fatalf("grants %+v", grants)
	}
	resumed := verifier.requests()[1].History
	if answer := resumed[len(resumed)-1].Content; !strings.Contains(answer, "here it is") || !strings.Contains(answer, "Attached to this delegation") {
		t.Fatalf("the resumed verifier's answer: %q", answer)
	}
}

// Artifacts on an action that attaches nothing are refused, not dropped: a send sends
// nothing, and an exit does not end the process.
func TestArtifactsOnAnActionThatAttachesNothingAreRefused(t *testing.T) {
	f := newEvidenceFixture(t)
	coordinator := &script{replies: []step{
		delegateStep("benchmarker"),
		replied(`{"action":{"type":"send","send":{"to":"pid-000001","body":"the record","artifacts":"%s"}}}`, f.latest),
		replied(`{"action":{"type":"send","send":{"to":"pid-000001","body":"the record","artifacts":["%s"]}}}`, f.latest),
		replied(`{"action":{"type":"exit","artifacts":"%s","exit_status":{"code":0,"reason":"done"}}}`, f.latest),
		finished,
	}}
	engine := f.engine(scripts{"coordinator": coordinator, "benchmarker": {replies: []step{publishFile(f), finished}}})
	spec := f.spec(nil, 4, nil)
	spec.Verification = nil
	if _, err := engine.Run(context.Background(), spec, agentflow.RunRequest{Goal: "verify"}); err != nil {
		t.Fatal(err)
	}
	if n := len(coordinator.requests()); n != 5 {
		t.Fatalf("the coordinator took %d steps, want 5: the refused exit must not end it", n)
	}
	if got := strings.Count(historyOf(coordinator), "tool_error name=send error=artifacts not attached"); got != 2 {
		t.Fatalf("%d sends refused, want 2:\n%s", got, historyOf(coordinator))
	}
	errors := toolErrorsOf(coordinator.requests())
	for _, want := range []string{"tool_error name=send error=artifacts not attached: only a delegate, spawn_agent and message_agent attach artifacts", "tool_error name=exit error=artifacts not attached"} {
		if !strings.Contains(errors, want) {
			t.Fatalf("lacks %q:\n%s", want, errors)
		}
	}
	for _, event := range f.events(t) {
		if event.Type == "ipc.sent" {
			t.Fatalf("mail was sent: %+v", event)
		}
	}
}

// A record made before delegate_attach replays as it ran: the action-level artifacts
// dropped unread, and the delegator told nothing.
func TestNoDelegateAttachReadsArtifactsInOnePlace(t *testing.T) {
	f := newEvidenceFixture(t)
	coordinator := &script{replies: []step{delegateStep("benchmarker"), replied(placements[1].reply, f.latest), finished}}
	verifier := &script{replies: []step{pass()}}
	engine := f.engine(scripts{"coordinator": coordinator, "benchmarker": {replies: []step{publishFile(f), finished}}, "verifier": verifier})
	engine.NoDelegateAttach = true
	summary, err := engine.Run(context.Background(), f.spec(rawRecord, 4, nil), agentflow.RunRequest{Goal: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.grants(t)) != 0 || summary.Verification.Status != agentflow.VerdictUnverified && summary.Verification.Status != agentflow.VerdictFail && summary.Verification.Status != agentflow.VerdictMissing {
		t.Fatalf("grants %+v, verdict %+v", f.grants(t), summary.Verification)
	}
	if history := historyOf(coordinator); strings.Contains(history, "attached=") {
		t.Fatalf("the delegator was told:\n%s", history)
	}
	for _, event := range f.events(t) {
		if event.Type == "run.started" && event.Data[agentflow.DelegateAttachField] != "" {
			t.Fatalf("run.started: %+v", event.Data)
		}
	}
}

func (f *evidenceFixture) events(t *testing.T) []kernel.Event {
	t.Helper()
	events, err := f.kernel.Events()
	if err != nil {
		t.Fatal(err)
	}
	return events
}
