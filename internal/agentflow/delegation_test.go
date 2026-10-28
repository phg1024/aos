package agentflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

// delegationSpec: a coordinator that writes only its ledger and may grant
// write_file and the workspace to a writer.
func delegationSpec(ws string) agentflow.WorkflowSpec {
	return agentflow.WorkflowSpec{
		Name:       "attenuated",
		EntryAgent: "coordinator",
		Agents: []agentflow.AgentSpec{
			{
				ID: "coordinator", SystemPrompt: "Coordinate.", AllowedAgents: []string{"writer"},
				Mounts:       []capability.Mount{{Name: "ws", HostPath: ws}, {Name: "ref", HostPath: ws + "-ref", ReadOnly: true}},
				Capabilities: capability.Set{Tools: []string{"spawn_agent", "read_file"}, ReadPaths: []string{ws, ws + "-ref"}, WritePaths: []string{ws + "/ledger.txt"}},
				Delegable:    capability.Set{Tools: []string{"write_file"}, WritePaths: []string{ws}},
			},
			{
				ID: "writer", SystemPrompt: "Write.",
				Mounts:       []capability.Mount{{Name: "ws", HostPath: ws}, {Name: "ref", HostPath: ws + "-ref", ReadOnly: true}},
				Capabilities: capability.Set{Tools: []string{"read_file", "write_file"}, ReadPaths: []string{ws}, WritePaths: []string{ws + "/src"}},
			},
		},
	}
}

func TestValidateRejectsChildWiderThanParent(t *testing.T) {
	if err := delegationSpec("/ws").Validate(); err != nil {
		t.Fatalf("a child within capabilities and delegable authority is valid: %v", err)
	}
	for name, tc := range map[string]struct {
		edit func(*agentflow.WorkflowSpec)
		want string
	}{
		"tool": {func(s *agentflow.WorkflowSpec) {
			s.Agents[1].Capabilities.Tools = append(s.Agents[1].Capabilities.Tools, "run")
		}, "coordinator -> writer exceeds what coordinator may grant: tool run"},
		"write path": {func(s *agentflow.WorkflowSpec) {
			s.Agents[1].Capabilities.WritePaths = []string{"/elsewhere"}
		}, "write path /elsewhere"},
		"writable mount under read-only": {func(s *agentflow.WorkflowSpec) {
			s.Agents[1].Mounts[1].ReadOnly = false
		}, "writable mount ref (/ws-ref) under a read-only parent mount"},
		"delegable": {func(s *agentflow.WorkflowSpec) {
			s.Agents[1].Delegable = capability.Set{Tools: []string{"web_fetch"}}
		}, "delegable tool web_fetch"},
	} {
		spec := delegationSpec("/ws")
		tc.edit(&spec)
		err := spec.Validate()
		if !errors.Is(err, agentflow.ErrWorkflowSpecInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
}

func TestDelegatedChildRecordHoldsAttenuatedSet(t *testing.T) {
	ws := t.TempDir()
	f := newExitFixture(t, &sequenceCompleter{actions: []map[string]any{delegateAction("writer"), exitWith(0, "wrote", false), exitWith(0, "done", false)}})
	summary, err := f.engine.Run(context.Background(), delegationSpec(ws), agentflow.RunRequest{Goal: "go"})
	if err != nil || summary.ExitStatus.Code != 0 {
		t.Fatalf("run: %+v %v", summary.ExitStatus, err)
	}
	child, err := f.kernel.Get(summary.AgentRuns[0].Children[0].PID)
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	if !reflect.DeepEqual(child.CapabilitySet.Tools, []string{"read_file", "write_file"}) || !reflect.DeepEqual(child.CapabilitySet.WritePaths, []string{ws + "/src"}) {
		t.Fatalf("child record = %+v", child.CapabilitySet)
	}
	if !child.Mounts[1].ReadOnly {
		t.Fatalf("the reference mount must stay read-only: %+v", child.Mounts)
	}

	// Defensively, the grant comes from the parent's kernel record even if
	// it is narrower than its spec.
	narrow, err := f.kernel.Spawn(kernel.ProcessSpec{
		Role: "agent:coordinator", Metadata: map[string]string{"agent_id": "coordinator"},
		Mounts:        []capability.Mount{{Name: "ws", HostPath: ws, ReadOnly: true}},
		CapabilitySet: capability.Set{Tools: []string{"spawn_agent", "read_file"}, ReadPaths: []string{ws}},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	spec := delegationSpec(ws)
	f.engine.Factory = agentflow.StaticBackendFactory{Completer: &sequenceCompleter{actions: []map[string]any{exitWith(0, "done", false)}}}
	handler := f.engine.DelegateHandler(spec, agentflow.RunRequest{Goal: "go"}, spec.Agents[0], nil)
	result, err := handler(context.Background(), narrow, runtime.DelegateCall{AgentID: "writer", Goal: "write"}, runtime.Session{})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	attenuated, err := f.kernel.Get(result.PID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want := capability.Set{Tools: []string{"read_file"}, ReadPaths: []string{ws}}
	if !reflect.DeepEqual(attenuated.CapabilitySet, want) || len(attenuated.Mounts) != 1 || !attenuated.Mounts[0].ReadOnly {
		t.Fatalf("child of a narrow parent got %+v mounts %+v", attenuated.CapabilitySet, attenuated.Mounts)
	}
}

// After narrowing, the rasterizer coordinator can write its ledger and
// summary but not the sources a verdict covered; its writer still can.
func TestRasterizerCoordinatorCannotWriteSources(t *testing.T) {
	ws := t.TempDir()
	raw, err := os.ReadFile(filepath.Join("..", "..", "workflows", "rasterizer-team.json"))
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), "/Users/claw/Code/aos/reports/rasterizer-team-demo", ws))
	var spec agentflow.WorkflowSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse: %v", err)
	}
	coordinator, _ := spec.Agent("coordinator")
	writer, _ := spec.Agent("writer")
	runtimeTools := tools.NewRuntime()
	write := func(agent agentflow.AgentSpec, caps capability.Set, path string) error {
		_, err := runtimeTools.Execute(tools.Request{Tool: "write_file", Args: map[string]string{"path": path, "content": "x"}, Mounts: agent.Mounts, CapabilitySet: caps})
		return err
	}
	if err := os.MkdirAll(filepath.Join(ws, "optimized", "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	source := filepath.Join(ws, "optimized", "src", "main.cpp")
	if err := write(coordinator, coordinator.Capabilities, source); !errors.Is(err, tools.ErrPathDenied) {
		t.Fatalf("coordinator writing a source: got %v, want ErrPathDenied", err)
	}
	for _, own := range []string{"optimized_coordinator_log.txt", "final-summary.txt"} {
		if err := write(coordinator, coordinator.Capabilities, filepath.Join(ws, own)); err != nil {
			t.Fatalf("coordinator writing %s: %v", own, err)
		}
	}
	grantable := capability.Union(coordinator.Capabilities, coordinator.Delegable)
	if err := write(writer, capability.DeriveChildSet(grantable, writer.Capabilities), source); err != nil {
		t.Fatalf("the writer keeps its delegated write access: %v", err)
	}
}
