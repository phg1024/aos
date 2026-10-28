package inspect

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/inference"
	"aos/internal/kernel"
	"aos/internal/runlog"
	"aos/internal/workflow"
)

// buildState lays down a small run by hand: a root with two children, one
// exited with checkpoints and one still running, a verdict, two model
// calls and three trace records.
func buildState(t *testing.T) string {
	t.Helper()
	stateDir := t.TempDir()
	k, err := kernel.New(filepath.Join(stateDir, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(filepath.Join(stateDir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	must := func(_ any, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	root, err := k.Spawn(kernel.ProcessSpec{Role: "workflow:demo"})
	must(root, err)
	done, err := k.Spawn(kernel.ProcessSpec{ParentPID: root.PID, Role: "agent:writer", Metadata: map[string]string{"agent_id": "writer"}})
	must(done, err)
	running, err := k.Spawn(kernel.ProcessSpec{ParentPID: root.PID, Role: "agent:checker", Metadata: map[string]string{"agent_id": "checker"}})
	must(running, err)
	must(k.Transition(running.PID, kernel.StateRunning))
	first, err := store.CreateJSON("checkpoint", done.PID, map[string]string{"step": "1"}, nil, nil)
	must(first, err)
	second, err := store.CreateJSON("checkpoint", done.PID, map[string]string{"step": "2"}, []string{first.ID}, nil)
	must(second, err)
	must(k.Exit(done.PID, kernel.ExitStatus{Code: 0, Reason: "wrote it", Origin: kernel.ExitOriginAgent}))
	must(store.CreateJSON("verification", root.PID, agentflow.Verdict{Policy: workflow.VerificationRequired, Verifier: "checker", Status: agentflow.VerdictMissing, Reason: "no verifier run"}, nil, nil))

	calls, err := runlog.Open(runlog.Path(stateDir, runlog.CallsFile))
	must(calls, err)
	for i, call := range []inference.CallRecord{
		{PID: done.PID, AgentID: "writer", PromptTokens: 100, CachedTokens: 40, CompletionTokens: 7, LatencyMS: 1200},
		{PID: done.PID, AgentID: "writer", Repair: true, PromptTokens: 30, CompletionTokens: 3, LatencyMS: 300},
		{PID: running.PID, AgentID: "checker", Error: "timeout", LatencyMS: 5000},
	} {
		must(calls.Append(func(seq int) any { call.Seq = seq; call.Time = time.Unix(int64(i), 0); return call }))
	}
	trace, err := runlog.Open(runlog.Path(stateDir, runlog.TraceFile))
	must(trace, err)
	sink := agentflow.TraceToLog(trace)
	sink(agentflow.TraceEvent{AgentID: "writer", PID: done.PID, Kind: "start", Detail: "draft", KernelEvent: "evt-000002"})
	sink(agentflow.TraceEvent{AgentID: "writer", PID: done.PID, Kind: "exit", Detail: "code=0"})
	sink(agentflow.TraceEvent{AgentID: "checker", PID: running.PID, Kind: "start", Detail: "check"})
	return stateDir
}

func TestLoad(t *testing.T) {
	report, err := Load(buildState(t), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Processes) != 3 || report.Processes[0].Depth != 0 || report.Processes[1].AgentID != "writer" || report.Processes[2].AgentID != "checker" || report.Processes[2].Depth != 1 {
		t.Fatalf("processes = %+v", report.Processes)
	}
	writer := report.Processes[1]
	if writer.State != "exited" || writer.Exit.Reason != "wrote it" || writer.LatestCheckpoint != "art-000002" {
		t.Fatalf("writer = %+v", writer)
	}
	if strings.Join(report.NotExited, ",") != "pid-000001,pid-000003" {
		t.Fatalf("not exited = %v", report.NotExited)
	}
	if report.Verification == nil || report.Verification.Status != agentflow.VerdictMissing {
		t.Fatalf("verification = %+v", report.Verification)
	}
	want := Usage{Calls: 3, Repairs: 1, Errors: 1, PromptTokens: 130, CachedTokens: 40, CompletionTokens: 10, LatencyMS: 6500}
	if report.TotalUsage != want {
		t.Fatalf("total = %+v", report.TotalUsage)
	}
	if len(report.Usage) != 2 || report.Usage[0].Calls != 2 || report.Usage[1].Errors != 1 {
		t.Fatalf("per process = %+v", report.Usage)
	}
	if report.TraceCount != 3 || len(report.TraceTail) != 2 || report.TraceTail[0].Seq != 2 {
		t.Fatalf("trace tail = %+v of %d", report.TraceTail, report.TraceCount)
	}
}

func TestLoadNegativeTailKeepsEverything(t *testing.T) {
	report, err := Load(buildState(t), -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.TraceTail) != 3 || report.TraceTail[0].KernelEvent != "evt-000002" {
		t.Fatalf("trace = %+v", report.TraceTail)
	}
}

func TestLoadRejectsNonStateDir(t *testing.T) {
	if _, err := Load(t.TempDir(), 10); err == nil || !strings.Contains(err.Error(), "not an aos state directory") {
		t.Fatalf("err = %v", err)
	}
}

func TestWriters(t *testing.T) {
	report, err := Load(buildState(t), 10)
	if err != nil {
		t.Fatal(err)
	}
	var text bytes.Buffer
	report.WriteText(&text)
	for _, want := range []string{"pid-000002 writer  exited  code=0 origin=agent \"wrote it\"  checkpoint art-000002", "not exited (no exit record): pid-000001, pid-000003", "verification (required", "total", "calls=3", "[evt-000002]"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("text output lacks %q:\n%s", want, text.String())
		}
	}
	var encoded bytes.Buffer
	if err := report.WriteJSON(&encoded); err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil || decoded.TotalUsage != report.TotalUsage || len(decoded.Processes) != 3 {
		t.Fatalf("round trip: %v %+v", err, decoded)
	}
}
