package agentflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/runtime"
	"aos/internal/skills"
	"aos/internal/tools"
)

var ErrUnknownAgent = errors.New("unknown agent")
var ErrDelegationDenied = errors.New("delegation denied")
var ErrDelegateAlreadyActive = errors.New("delegate already active")

type BackendFactory interface {
	BackendFor(agent AgentSpec, workflow WorkflowSpec) (runtime.Backend, error)
}

type StaticBackendFactory struct {
	Completer  JSONCompleter
	Skills     skills.Registry
	Compaction runtime.CompactionPolicy
}

func (f StaticBackendFactory) BackendFor(agent AgentSpec, workflow WorkflowSpec) (runtime.Backend, error) {
	loadedSkills, err := f.Skills.LoadMany(agent.Skills)
	if err != nil {
		return nil, err
	}
	return PromptBackend{
		Completer:  f.Completer,
		Workflow:   workflow,
		Agent:      agent,
		Skills:     loadedSkills,
		Compaction: f.Compaction,
	}, nil
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
	Leases    *lease.Manager
	Tools     *tools.Runtime
	Factory   BackendFactory
	Trace     func(TraceEvent)
	// Compaction is passed to every agent runtime; zero uses runtime defaults.
	Compaction runtime.CompactionPolicy
}

type TraceEvent struct {
	Time    time.Time
	AgentID string
	PID     string
	Kind    string
	Detail  string
}

func (e *Engine) emitTrace(agentID, pid, kind, detail string) {
	if e.Trace == nil {
		return
	}
	e.Trace(TraceEvent{
		Time:    time.Now(),
		AgentID: agentID,
		PID:     pid,
		Kind:    kind,
		Detail:  detail,
	})
}

func NewStdoutTracer() func(TraceEvent) {
	var mu sync.Mutex
	useColor := stdoutSupportsANSI()
	return func(event TraceEvent) {
		mu.Lock()
		defer mu.Unlock()
		timestamp := event.Time.Format("15:04:05")
		agent := event.AgentID
		if agent == "" {
			agent = "workflow"
		}
		pid := event.PID
		if pid == "" {
			pid = "-"
		}
		header := fmt.Sprintf(
			"%s %s %s",
			traceColor(useColor, traceColorDim, "["+timestamp+"]"),
			traceColor(useColor, traceColorAgent, agent),
			traceColor(useColor, traceColorDim, "(pid="+pid+")"),
		)
		kind := traceColor(useColor, traceKindColor(event.Kind), strings.ToUpper(event.Kind))
		fmt.Fprintf(os.Stdout, "%s %s\n", header, kind)
		if detail := formatTraceDetail(event.Detail); detail != "" {
			fmt.Fprintf(os.Stdout, "  %s\n", detail)
		}
	}
}

const (
	traceColorReset  = "\033[0m"
	traceColorDim    = "\033[2m"
	traceColorAgent  = "\033[1;36m"
	traceColorStart  = "\033[1;32m"
	traceColorMsg    = "\033[1;37m"
	traceColorTool   = "\033[1;34m"
	traceColorResult = "\033[1;32m"
	traceColorError  = "\033[1;31m"
	traceColorExit   = "\033[1;35m"
	traceColorInfo   = "\033[1;33m"
)

func stdoutSupportsANSI() bool {
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

func traceColor(enabled bool, colorCode, text string) string {
	if !enabled || text == "" {
		return text
	}
	return colorCode + text + traceColorReset
}

func traceKindColor(kind string) string {
	switch kind {
	case "start":
		return traceColorStart
	case "message":
		return traceColorMsg
	case "tool_call":
		return traceColorTool
	case "tool_result":
		return traceColorResult
	case "tool_error", "runtime_error":
		return traceColorError
	case "delegate", "delegate_result":
		return traceColorInfo
	case "exit":
		return traceColorExit
	default:
		return traceColorMsg
	}
}

func formatTraceDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return ""
	}
	lines := strings.Split(detail, "\n")
	for i, line := range lines {
		line = strings.Join(strings.Fields(strings.TrimSpace(line)), " ")
		if len(line) > 140 {
			line = line[:137] + "..."
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n  ")
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
	e.emitTrace(agent.ID, process.PID, "start", req.Goal)

	var childRuns []AgentRun
	agentRuntime := &runtime.AgentRuntime{
		Kernel:     e.Kernel,
		Artifacts:  e.Artifacts,
		Tools:      e.Tools,
		Bus:        e.Bus,
		Leases:     e.Leases,
		MaxSteps:   maxSteps(agent),
		Compaction: e.Compaction,
		DelegateHandler: e.DelegateHandler(spec, req, agent, func(child AgentRun) {
			childRuns = append(childRuns, child)
		}),
	}
	backend, err := e.Factory.BackendFor(agent, spec)
	if err != nil {
		return AgentRun{}, err
	}
	agentRuntime.Backend = backend

	initial := []runtime.Message{{Role: "user", Content: req.Goal}}
	result, err := agentRuntime.RunWithOptions(ctx, process.PID, initial, runtime.RunOptions{
		ContinueOnToolError: true,
		OnAssistantMessage: func(message string) {
			e.emitTrace(agent.ID, process.PID, "message", message)
		},
		OnToolCall: func(call runtime.ToolCall) {
			e.emitTrace(agent.ID, process.PID, "tool_call", formatToolCall(call))
		},
		OnToolResult: func(result tools.Result) {
			e.emitTrace(agent.ID, process.PID, "tool_result", formatToolResult(result))
		},
		OnToolError: func(call runtime.ToolCall, err error) {
			e.emitTrace(agent.ID, process.PID, "tool_error", fmt.Sprintf("%s error=%s", formatToolCall(call), err.Error()))
		},
	})
	if err != nil {
		e.emitTrace(agent.ID, process.PID, "runtime_error", err.Error())
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
	e.emitTrace(agent.ID, process.PID, "exit", fmt.Sprintf("code=%d reason=%s retryable=%t", run.ExitStatus.Code, run.ExitStatus.Reason, run.ExitStatus.Retryable))
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

// DelegateHandler lets agent delegate to the workflow agents it may call.
// Each child runs to completion through the engine; onChild, when set,
// receives every finished child run.
func (e *Engine) DelegateHandler(spec WorkflowSpec, req RunRequest, agent AgentSpec, onChild func(AgentRun)) runtime.DelegateHandler {
	return func(ctx context.Context, parent kernel.Process, call runtime.DelegateCall, session runtime.Session) (runtime.DelegateResult, error) {
		if !contains(agent.AllowedAgents, call.AgentID) {
			return runtime.DelegateResult{}, fmt.Errorf("%w: %s -> %s", ErrDelegationDenied, agent.ID, call.AgentID)
		}
		e.emitTrace(agent.ID, parent.PID, "delegate", fmt.Sprintf("to=%s goal=%s", call.AgentID, strings.TrimSpace(call.Goal)))
		children, err := e.Kernel.Children(parent.PID)
		if err != nil {
			return runtime.DelegateResult{}, err
		}
		for _, child := range children {
			if child.State == kernel.StateExited {
				continue
			}
			if child.Metadata["agent_id"] == call.AgentID {
				return runtime.DelegateResult{}, fmt.Errorf("%w: %s", ErrDelegateAlreadyActive, call.AgentID)
			}
		}
		childReq := req
		if strings.TrimSpace(call.Goal) != "" {
			childReq.Goal = call.Goal
		}
		childRun, err := e.runAgent(ctx, spec, childReq, call.AgentID, parent.PID)
		if err != nil {
			return runtime.DelegateResult{}, err
		}
		if onChild != nil {
			onChild(childRun)
		}
		e.emitTrace(agent.ID, parent.PID, "delegate_result", fmt.Sprintf("from=%s pid=%s reason=%s", childRun.AgentID, childRun.PID, childRun.ExitStatus.Reason))
		return runtime.DelegateResult{
			AgentID:      childRun.AgentID,
			PID:          childRun.PID,
			Summary:      childRun.FinalMessage,
			ResultID:     childRun.ResultID,
			CheckpointID: childRun.CheckpointID,
		}, nil
	}
}

func formatToolCall(call runtime.ToolCall) string {
	if len(call.Args) == 0 {
		return call.Name
	}
	keys := make([]string, 0, len(call.Args))
	for key := range call.Args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", key, summarizeTraceText(call.Args[key])))
	}
	return fmt.Sprintf("%s %s", call.Name, strings.Join(parts, " "))
}

func formatToolResult(result tools.Result) string {
	if len(result.Output) == 0 {
		if result.Summary != "" {
			return summarizeTraceText(result.Summary)
		}
		return result.Tool
	}
	keys := make([]string, 0, len(result.Output))
	for key := range result.Output {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, min(3, len(keys)))
	for _, key := range keys[:min(3, len(keys))] {
		parts = append(parts, fmt.Sprintf("%s=%s", key, summarizeTraceText(result.Output[key])))
	}
	return fmt.Sprintf("%s %s", result.Tool, strings.Join(parts, " "))
}

func summarizeTraceText(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if len(value) > 120 {
		return value[:117] + "..."
	}
	return value
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
