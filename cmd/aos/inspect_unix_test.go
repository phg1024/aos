//go:build unix

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/inference"
	"aos/internal/inspect"
	"aos/internal/kernel"
	"aos/internal/runlog"
	"aos/internal/runtime"
	"aos/internal/statedir"
)

const killHelperStateEnv = "AOS_KILL_HELPER_STATE"

// scriptedServer answers chat completions from script in order, reporting
// usage, and blocks forever once the script runs out.
func scriptedServer(script []map[string]any) *httptest.Server {
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		if n > len(script) {
			<-r.Context().Done()
			select {}
		}
		content, _ := json.Marshal(map[string]any{"action": script[n-1]})
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": string(content)}}},
			"usage":   map[string]any{"prompt_tokens": 100 * n, "completion_tokens": n, "prompt_tokens_details": map[string]any{"cached_tokens": 50}},
		})
	}))
}

// TestKillHelperProcess runs a workflow that hangs on its fifth model call.
func TestKillHelperProcess(t *testing.T) {
	stateDir := os.Getenv(killHelperStateEnv)
	if stateDir == "" {
		t.Skip("subprocess helper")
	}
	server := scriptedServer([]map[string]any{
		{"type": "delegate", "delegate": map[string]any{"agent_id": "writer", "goal": "draft"}},
		{"type": "message", "message": "draft text"},
		{"type": "exit", "exit_status": map[string]any{"code": 0, "reason": "drafted"}},
		{"type": "delegate", "delegate": map[string]any{"agent_id": "writer", "goal": "revise"}},
	})
	defer server.Close()
	workflow := filepath.Join(filepath.Dir(stateDir), "workflow.json")
	backend := inference.NewVLLMBackend(inference.VLLMConfig{BaseURL: server.URL, Model: "scripted", Timeout: time.Hour})
	_ = runWorkflow(context.Background(), backend, runtime.CompactionPolicy{}, capability.Budget{}, workflow, nil, "", "", stateDir, "go", false)
}

// A run killed mid-step can be reconstructed from disk: the trace and call
// ledger hold everything up to the last completed step, and inspect rebuilds
// the tree, checkpoints and usage without any model.
func TestInspectReconstructsKilledRun(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	spec := `{"name":"killed","entry_agent":"coordinator","agents":[
		{"id":"coordinator","system_prompt":"Coordinate.","allowed_agents":["writer"],"max_steps":6,"capabilities":{"tools":["spawn_agent"]}},
		{"id":"writer","system_prompt":"Write.","max_steps":4}]}`
	if err := os.WriteFile(filepath.Join(root, "workflow.json"), []byte(spec), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestKillHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), killHelperStateEnv+"="+stateDir)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	callsPath := runlog.Path(stateDir, runlog.CallsFile)
	tracePath := runlog.Path(stateDir, runlog.TraceFile)
	deadline := time.Now().Add(30 * time.Second)
	for {
		calls, _ := runlog.Read[inference.CallRecord](callsPath)
		trace, _ := runlog.Read[agentflow.TraceRecord](tracePath)
		starts := 0
		for _, event := range trace {
			if event.Kind == "start" {
				starts++
			}
		}
		if len(calls) == 4 && starts == 3 {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("run never reached the hanging step: %d calls, %d starts", len(calls), starts)
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // let the fifth call reach the server
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_ = cmd.Wait()

	report, err := inspect.Load(stateDir, -1)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	var tree []string
	for _, process := range report.Processes {
		tree = append(tree, strings.Repeat(" ", process.Depth)+process.AgentID+":"+process.State)
	}
	if got := strings.Join(tree, ","); got != ":running, coordinator:running,  writer:exited,  writer:running" {
		t.Fatalf("process tree = %q", got)
	}
	if len(report.NotExited) != 3 {
		t.Fatalf("root, coordinator and the second writer never exited: %v", report.NotExited)
	}
	firstWriter := report.Processes[2]
	if firstWriter.Exit == nil || firstWriter.Exit.Reason != "drafted" || firstWriter.Exit.Origin != kernel.ExitOriginAgent || firstWriter.LatestCheckpoint == "" {
		t.Fatalf("first writer = %+v", firstWriter)
	}
	if report.Processes[1].LatestCheckpoint == "" {
		t.Fatal("the coordinator's latest checkpoint should be found")
	}
	if report.TotalUsage.Calls != 4 || report.TotalUsage.PromptTokens != 100+200+300+400 || report.TotalUsage.CompletionTokens != 10 || report.TotalUsage.CachedTokens != 200 {
		t.Fatalf("usage totals = %+v", report.TotalUsage)
	}
	if len(report.Usage) != 2 || report.Usage[0].AgentID != "coordinator" || report.Usage[0].Calls != 2 || report.Usage[1].Calls != 2 {
		t.Fatalf("per-process usage = %+v", report.Usage)
	}
	kinds := []string{}
	joined := 0
	for _, event := range report.TraceTail {
		kinds = append(kinds, event.AgentID+":"+event.Kind)
		if event.KernelEvent != "" {
			joined++
		}
	}
	want := "coordinator:start,coordinator:delegate,writer:start,writer:message,writer:exit,coordinator:delegate_result,coordinator:delegate,writer:start"
	if strings.Join(kinds, ",") != want {
		t.Fatalf("trace = %s\nwant    %s", strings.Join(kinds, ","), want)
	}
	if joined != 4 {
		t.Fatalf("start and exit events should name their kernel events, %d did", joined)
	}
	if report.RunActive {
		t.Fatal("the killed run no longer holds the state directory")
	}

	// With the run gone, reap closes the three orphans so they can be
	// recovered, each exit naming its latest checkpoint.
	reaped, err := statedir.Reap(stateDir)
	if err != nil || strings.Join(reaped, ",") != "pid-000004,pid-000002,pid-000001" {
		t.Fatalf("reap = %v, %v", reaped, err)
	}
	if after, _ := inspect.Load(stateDir, 0); len(after.NotExited) != 0 {
		t.Fatalf("still not exited after reap: %v", after.NotExited)
	}
	k, _ := kernel.New(filepath.Join(stateDir, "kernel"))
	coordinator, _ := k.Await("pid-000002")
	if !coordinator.Retryable || coordinator.Reason != statedir.OrphanReason || coordinator.Checkpoint != report.Processes[1].LatestCheckpoint {
		t.Fatalf("reaped coordinator = %+v", coordinator)
	}
	store, _ := artifact.New(filepath.Join(stateDir, "artifacts"))
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Backend: &runtime.FakeBackend{}}
	recovered, err := agent.Recover(context.Background(), "pid-000002", coordinator.Checkpoint, nil, runtime.RunOptions{})
	if err != nil || recovered.PID == "pid-000002" || recovered.ExitStatus.Code != 0 {
		t.Fatalf("recover after reap = %+v, %v", recovered, err)
	}
	if again, err := statedir.Reap(stateDir); err != nil || len(again) != 0 {
		t.Fatalf("a second reap found %v, %v", again, err)
	}
}
