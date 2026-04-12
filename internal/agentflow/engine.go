package agentflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

var ErrUnknownAgent = errors.New("unknown agent")
var ErrDelegationDenied = errors.New("delegation denied")

type BackendFactory interface {
	BackendFor(agent AgentSpec, workflow WorkflowSpec) runtime.Backend
}

type StaticBackendFactory struct {
	Completer JSONCompleter
}

func (f StaticBackendFactory) BackendFor(agent AgentSpec, workflow WorkflowSpec) runtime.Backend {
	return PromptBackend{
		Completer: f.Completer,
		Workflow:  workflow,
		Agent:     agent,
	}
}

type RunRequest struct {
	Goal       string
	InputPaths []string
	OutputPath string
}

type AgentRun struct {
	AgentID       string            `json:"agent_id"`
	PID           string            `json:"pid"`
	ParentPID     string            `json:"parent_pid,omitempty"`
	CheckpointID  string            `json:"checkpoint_id,omitempty"`
	ResultID      string            `json:"result_id,omitempty"`
	ExitStatus    kernel.ExitStatus `json:"exit_status"`
	FinalMessage  string            `json:"final_message,omitempty"`
	ToolArtifacts []string          `json:"tool_artifacts,omitempty"`
	Delegates     []runtime.Message `json:"delegates,omitempty"`
	Children      []AgentRun        `json:"children,omitempty"`
}

type RunSummary struct {
	Workflow   string     `json:"workflow"`
	RootPID    string     `json:"root_pid"`
	EntryPID   string     `json:"entry_pid"`
	ResultID   string     `json:"result_id,omitempty"`
	AgentRuns  []AgentRun `json:"agent_runs"`
	OutputPath string     `json:"output_path,omitempty"`
}

type Engine struct {
	Kernel    *kernel.Kernel
	Artifacts *artifact.Store
	Bus       *ipc.Bus
	Tools     *tools.Runtime
	Factory   BackendFactory
}

func (e *Engine) Run(ctx context.Context, spec WorkflowSpec, req RunRequest) (RunSummary, error) {
	if err := spec.Validate(); err != nil {
		return RunSummary{}, err
	}
	root, err := e.Kernel.Spawn(kernel.ProcessSpec{
		Role:     "workflow:" + spec.Name,
		Metadata: map[string]string{"goal": req.Goal},
	})
	if err != nil {
		return RunSummary{}, err
	}
	summary := RunSummary{
		Workflow:   spec.Name,
		RootPID:    root.PID,
		OutputPath: req.OutputPath,
	}
	run, err := e.runAgent(ctx, spec, req, spec.EntryAgent, root.PID)
	if err != nil {
		_, _ = e.Kernel.Exit(root.PID, kernel.ExitStatus{Code: 1, Reason: err.Error(), Retryable: false})
		return RunSummary{}, err
	}
	summary.EntryPID = run.PID
	summary.ResultID = run.ResultID
	summary.AgentRuns = append(summary.AgentRuns, run)

	allRuns, err := e.Artifacts.CreateJSON("workflow_summary", root.PID, summary, nil, map[string]string{
		"workflow": spec.Name,
	})
	if err != nil {
		return RunSummary{}, err
	}
	summary.ResultID = allRuns.ID
	if _, err := e.Kernel.Exit(root.PID, kernel.ExitStatus{Code: 0, Reason: "workflow completed", Retryable: false}); err != nil {
		return RunSummary{}, err
	}
	return summary, nil
}

func (e *Engine) runAgent(ctx context.Context, spec WorkflowSpec, req RunRequest, agentID, parentPID string) (AgentRun, error) {
	agent, ok := spec.Agent(agentID)
	if !ok {
		return AgentRun{}, fmt.Errorf("%w: %s", ErrUnknownAgent, agentID)
	}
	process, err := e.Kernel.Spawn(kernel.ProcessSpec{
		ParentPID:     parentPID,
		Role:          "agent:" + agent.ID,
		Metadata:      agentMetadata(agent, req),
		Mounts:        agentMounts(agent, req),
		CapabilitySet: agentCapabilities(agent, req),
	})
	if err != nil {
		return AgentRun{}, err
	}

	var childRuns []AgentRun
	agentRuntime := &runtime.AgentRuntime{
		Kernel:    e.Kernel,
		Artifacts: e.Artifacts,
		Tools:     e.Tools,
		Bus:       e.Bus,
		Backend:   e.Factory.BackendFor(agent, spec),
		MaxSteps:  maxSteps(agent),
		DelegateHandler: func(ctx context.Context, parent kernel.Process, call runtime.DelegateCall, session runtime.Session) (runtime.DelegateResult, error) {
			if !contains(agent.AllowedAgents, call.AgentID) {
				return runtime.DelegateResult{}, fmt.Errorf("%w: %s -> %s", ErrDelegationDenied, agent.ID, call.AgentID)
			}
			childReq := req
			if strings.TrimSpace(call.Goal) != "" {
				childReq.Goal = call.Goal
			}
			childRun, err := e.runAgent(ctx, spec, childReq, call.AgentID, parent.PID)
			if err != nil {
				return runtime.DelegateResult{}, err
			}
			childRuns = append(childRuns, childRun)
			return runtime.DelegateResult{
				AgentID:      childRun.AgentID,
				PID:          childRun.PID,
				Summary:      childRun.FinalMessage,
				ResultID:     childRun.ResultID,
				CheckpointID: childRun.CheckpointID,
			}, nil
		},
	}

	initial := []runtime.Message{{Role: "user", Content: req.Goal}}
	result, err := agentRuntime.Run(ctx, process.PID, initial)
	if err != nil {
		return AgentRun{}, err
	}
	session, err := agentRuntime.RestoreCheckpoint(result.CheckpointID)
	if err != nil {
		return AgentRun{}, err
	}
	run := AgentRun{
		AgentID:       agent.ID,
		PID:           process.PID,
		ParentPID:     parentPID,
		CheckpointID:  result.CheckpointID,
		ExitStatus:    result.ExitStatus,
		FinalMessage:  finalAssistantMessage(session.Messages),
		ToolArtifacts: append([]string(nil), session.ToolArtifactIDs...),
	}
	run.Delegates = delegateMessages(session.Messages)
	run.Children = append(run.Children, childRuns...)
	for _, child := range childRuns {
		run.ResultID = child.ResultID
	}
	artifactRecord, err := e.Artifacts.CreateJSON("agent_run", process.PID, run, nil, map[string]string{
		"agent_id": agent.ID,
	})
	if err != nil {
		return AgentRun{}, err
	}
	run.ResultID = artifactRecord.ID
	for _, child := range childRuns {
		run.Delegates = append(run.Delegates, runtime.Message{
			Role:    "delegate",
			Content: child.AgentID + ":" + child.ResultID,
		})
	}
	return run, nil
}

func agentMetadata(agent AgentSpec, req RunRequest) map[string]string {
	metadata := cloneStringMap(agent.Metadata)
	if metadata == nil {
		metadata = map[string]string{}
	}
	metadata["agent_id"] = agent.ID
	metadata["goal"] = req.Goal
	metadata["input_paths"] = strings.Join(req.InputPaths, "\n")
	metadata["output_path"] = req.OutputPath
	return metadata
}

func agentMounts(agent AgentSpec, req RunRequest) []capability.Mount {
	if len(agent.Mounts) > 0 {
		return append([]capability.Mount(nil), agent.Mounts...)
	}
	return deriveMounts(req.InputPaths, req.OutputPath)
}

func agentCapabilities(agent AgentSpec, req RunRequest) capability.Set {
	set := agent.Capabilities
	if len(set.ReadPaths) == 0 || len(set.WritePaths) == 0 {
		_, derived := deriveMountsAndCapabilities(req.InputPaths, req.OutputPath)
		if len(set.ReadPaths) == 0 {
			set.ReadPaths = derived.ReadPaths
		}
		if len(set.WritePaths) == 0 {
			set.WritePaths = derived.WritePaths
		}
	}
	return set
}

func maxSteps(agent AgentSpec) int {
	if agent.MaxSteps > 0 {
		return agent.MaxSteps
	}
	return 24
}

func finalAssistantMessage(messages []runtime.Message) string {
	for idx := len(messages) - 1; idx >= 0; idx-- {
		if messages[idx].Role == "assistant" {
			return messages[idx].Content
		}
	}
	return ""
}

func delegateMessages(messages []runtime.Message) []runtime.Message {
	var out []runtime.Message
	for _, message := range messages {
		if message.Role == "tool" && strings.HasPrefix(message.Content, "delegate_result ") {
			out = append(out, message)
		}
	}
	return out
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func deriveMounts(inputPaths []string, outputPath string) []capability.Mount {
	mounts, _ := deriveMountsAndCapabilities(inputPaths, outputPath)
	return mounts
}

func deriveMountsAndCapabilities(inputPaths []string, outputPath string) ([]capability.Mount, capability.Set) {
	mountIndex := map[string]int{}
	var mounts []capability.Mount
	var readPaths []string
	var writePaths []string

	for idx, inputPath := range inputPaths {
		if inputPath == "" {
			continue
		}
		dir := cleanDir(inputPath)
		if _, ok := mountIndex[dir]; !ok {
			mountIndex[dir] = len(mounts)
			mounts = append(mounts, capability.Mount{
				Name:     fmt.Sprintf("input-%d", idx),
				HostPath: dir,
				ReadOnly: true,
			})
		}
		readPaths = append(readPaths, dir)
	}

	if outputPath != "" {
		outputDir := cleanDir(outputPath)
		if index, ok := mountIndex[outputDir]; ok {
			mounts[index].ReadOnly = false
		} else {
			for index, mount := range mounts {
				if pathWithin(mount.HostPath, outputDir) {
					mounts[index].ReadOnly = false
				}
			}
			mounts = append(mounts, capability.Mount{
				Name:     "output",
				HostPath: outputDir,
				ReadOnly: false,
			})
		}
		readPaths = append(readPaths, outputDir)
		writePaths = append(writePaths, outputDir)
	}

	return mounts, capability.Set{
		ReadPaths:  uniqueStrings(readPaths),
		WritePaths: uniqueStrings(writePaths),
	}
}

func cleanDir(path string) string {
	return filepath.Clean(filepath.Dir(path))
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func pathWithin(base, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && rel != "..")
}
