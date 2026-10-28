package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/inference"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/runtime"
	"aos/internal/skills"
	"aos/internal/tokenizer"
	"aos/internal/tools"
	"aos/internal/tui/render"
)

func main() {
	var workflowPath string
	var agentID string
	var systemPrompt string
	var stateDir string
	var workdir string
	var toolList string
	var skillDirs sliceFlags
	var skillRefs sliceFlags

	flag.StringVar(&workflowPath, "workflow", "", "optional workflow JSON path")
	flag.StringVar(&agentID, "agent", "", "agent ID when using a workflow")
	flag.StringVar(&systemPrompt, "system-prompt", "You are a helpful AOS agent in an interactive terminal. Respond conversationally, inspect the workspace with list_dir or find_files before assuming file paths, use tools when useful, keep internal exploration in tool actions instead of visible chatter, use wait when you need more user input, and before exiting after completing a user task always send one final user-facing message that summarizes the outcome.", "system prompt for direct interactive mode")
	flag.StringVar(&stateDir, "state-dir", "", "optional persistent state directory")
	flag.StringVar(&workdir, "workdir", ".", "workspace directory for read/write tools")
	flag.StringVar(&toolList, "tools", "list_dir,find_files,read_file,write_file,run,web_search,web_fetch", "comma-separated tools for direct interactive mode")
	flag.Var(&skillDirs, "skill-dir", "directory containing Claude-Code-style skills (repeatable)")
	flag.Var(&skillRefs, "skill", "skill name or skill path to load (repeatable)")
	flag.Parse()

	ui := render.NewPalette(os.Stdout)

	if stateDir == "" {
		var err error
		stateDir, err = os.MkdirTemp("", "aos-tui-*")
		if err != nil {
			fail(err)
		}
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		fail(err)
	}

	absWorkdir, err := filepath.Abs(workdir)
	if err != nil {
		fail(err)
	}

	spec, agent := loadInteractiveSpec(workflowPath, agentID, systemPrompt, absWorkdir, parseTools(toolList), []string(skillDirs), []string(skillRefs))

	k, err := kernel.New(filepath.Join(stateDir, "kernel"))
	if err != nil {
		fail(err)
	}
	store, err := artifact.New(filepath.Join(stateDir, "artifacts"))
	if err != nil {
		fail(err)
	}
	leaseManager, err := lease.New(filepath.Join(stateDir, "leases"))
	if err != nil {
		fail(err)
	}
	bus, err := ipc.New(filepath.Join(stateDir, "bus"))
	if err != nil {
		fail(err)
	}

	process, err := k.Spawn(kernel.ProcessSpec{
		Role:          "agent:" + agent.ID,
		Metadata:      map[string]string{"agent_id": agent.ID, "goal": "Interactive terminal session", "workdir": absWorkdir},
		Mounts:        agentMounts(agent, absWorkdir),
		CapabilitySet: agentCapabilities(agent, absWorkdir),
	})
	if err != nil {
		fail(err)
	}

	llmConfig := inference.LoadBackendConfig(180 * time.Second)
	modelName := llmConfig.Model
	repoRoot := detectRepoRoot(absWorkdir)
	stream := render.NewStreamRenderer(ui)
	backend := inference.NewBackendFromConfig(llmConfig, inference.BackendOptions{
		OnStreamEvent: stream.Handle,
	})

	compaction := llmConfig.CompactionPolicy()
	agentRuntime := &runtime.AgentRuntime{
		Kernel:     k,
		Artifacts:  store,
		Tools:      tools.NewRuntime(),
		Bus:        bus,
		Leases:     leaseManager,
		MaxSteps:   maxSteps(agent),
		Compaction: compaction,
	}
	loadedSkills, err := skills.NewRegistry(spec.SkillRoots).LoadMany(agent.Skills)
	if err != nil {
		fail(err)
	}
	agentRuntime.Backend = agentflow.PromptBackend{
		Completer:  backend,
		Workflow:   spec,
		Agent:      agent,
		Skills:     loadedSkills,
		Compaction: compaction,
	}

	footer := render.NewFooterBar(ui)
	defer footer.Unpin()
	observeRoot := func(kind, detail string) {}
	if workflowPath != "" {
		progress := render.NewWorkflowProgress(ui, spec)
		footer.SetProgress(progress)
		engine := &agentflow.Engine{
			Kernel:    k,
			Artifacts: store,
			Bus:       bus,
			Leases:    leaseManager,
			Tools:     agentRuntime.Tools,
			Factory: agentflow.StaticBackendFactory{
				Completer:  backend,
				Skills:     skills.NewRegistry(spec.SkillRoots),
				Compaction: compaction,
			},
			Compaction: compaction,
			Trace: func(event agentflow.TraceEvent) {
				if line := progress.Apply(event); line != "" {
					stream.PrintStatus(line)
					return
				}
				footer.Draw()
			},
		}
		agentRuntime.DelegateHandler = engine.DelegateHandler(spec, agentflow.RunRequest{Goal: process.Metadata["goal"]}, agent, nil)
		observeRoot = func(kind, detail string) {
			progress.Observe(agentflow.TraceEvent{Time: time.Now(), AgentID: agent.ID, PID: process.PID, Kind: kind, Detail: detail})
			footer.Draw()
		}
		stopTicker := startProgressTicker(progress, footer)
		defer stopTicker()
	}
	var tokenCounter tokenizer.Counter
	if sidecar, sidecarErr := tokenizer.Start(getenv("AOS_TOKENIZER_PYTHON", "python3"), repoRoot, modelName); sidecarErr == nil {
		tokenCounter = sidecar
		defer tokenCounter.Close()
	}

	fmt.Println(ui.Banner(render.AOSBanner()))
	fmt.Println(ui.Header(fmt.Sprintf("AOS TUI ready  agent=%s  pid=%s", agent.ID, process.PID)))
	fmt.Println(ui.Subtle("Backend: " + string(llmConfig.Kind)))
	fmt.Println(ui.Subtle("Model: " + modelName))
	if tokenCounter != nil {
		fmt.Println(ui.Subtle("Tokenizer: exact sidecar"))
	} else {
		fmt.Println(ui.Subtle("Tokenizer: heuristic fallback"))
	}
	fmt.Println(ui.Subtle("State: " + stateDir))
	fmt.Println(ui.Subtle("Commands: /quit, /state, /recover"))

	if workflowPath != "" {
		fmt.Println(ui.Subtle(fmt.Sprintf("Workflow: %s  entry=%s  delegates=%s", spec.Name, agent.ID, strings.Join(agent.AllowedAgents, ","))))
	}

	var checkpointID string
	var currentSession runtime.Session
	scanner := bufio.NewScanner(os.Stdin)
	for {
		footer.Update(render.FooterStatus{
			Process:      process,
			Model:        modelName,
			CheckpointID: checkpointID,
			Session:      currentSession,
			Counter:      tokenCounter,
			Backend:      agentRuntime.Backend.(agentflow.PromptBackend),
		})
		fmt.Print(ui.Prompt("you> "))
		footer.Draw()
		if !scanner.Scan() {
			fmt.Println()
			return
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		switch line {
		case "/quit":
			return
		case "/state":
			footer.Println(ui.Subtle(fmt.Sprintf("pid=%s checkpoint=%s state_dir=%s", process.PID, checkpointID, stateDir)))
			continue
		case "/recover":
			if checkpointID == "" {
				footer.Println(ui.Warn("no checkpoint yet"))
				continue
			}
			result, err := agentRuntime.Recover(context.Background(), process.PID, checkpointID, nil, interactiveRunOptions(ui, stream, footer, observeRoot))
			if err != nil && err != runtime.ErrAgentBlocked {
				stream.Clear()
				footer.Println(ui.Error("recover error: " + err.Error()))
				continue
			}
			if result.PID != "" {
				process.PID = result.PID
			}
			checkpointID = result.CheckpointID
			currentSession = result.Session
			stream.Clear()
			printResult(ui, footer, result)
			continue
		}

		if _, err := bus.Send("user", process.PID, line); err != nil {
			footer.Println(ui.Error("send error: " + err.Error()))
			continue
		}

		var result runtime.RunResult
		observeRoot("start", line)
		if checkpointID == "" {
			result, err = agentRuntime.RunWithOptions(context.Background(), process.PID, nil, interactiveRunOptions(ui, stream, footer, observeRoot))
		} else {
			result, err = agentRuntime.Resume(context.Background(), process.PID, checkpointID, nil, interactiveRunOptions(ui, stream, footer, observeRoot))
		}
		if err != nil && err != runtime.ErrAgentBlocked {
			stream.Clear()
			footer.Println(ui.Error("runtime error: " + err.Error()))
		}
		observeRoot("exit", "")
		checkpointID = result.CheckpointID
		currentSession = result.Session
		stream.Clear()
		printResult(ui, footer, result)
	}
}

// startProgressTicker redraws the footer every second while workflow agents
// run, so elapsed time keeps moving during long model calls.
func startProgressTicker(progress *render.WorkflowProgress, footer *render.FooterBar) func() {
	ticker := time.NewTicker(time.Second)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				if progress.Active() {
					footer.Draw()
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		ticker.Stop()
		close(done)
	}
}

func loadInteractiveSpec(workflowPath, requestedAgentID, systemPrompt, workdir string, tools, skillDirs, skillRefs []string) (agentflow.WorkflowSpec, agentflow.AgentSpec) {
	if workflowPath == "" {
		roots := defaultSkillRoots(workdir)
		roots = append(roots, normalizeSkillRoots(skillDirs, workdir)...)
		roots = dedupeStrings(roots)
		agent := agentflow.AgentSpec{
			ID:           "interactive",
			SystemPrompt: systemPrompt,
			Skills:       dedupeStrings(append([]string{"python-runner"}, skillRefs...)),
			MaxSteps:     32,
			Mounts: []capability.Mount{
				{Name: "workspace", HostPath: workdir},
			},
			Capabilities: capability.Set{
				ReadPaths:  []string{workdir},
				WritePaths: []string{workdir},
				Tools:      tools,
			},
		}
		return agentflow.WorkflowSpec{
			SkillRoots: roots,
			Name:       "interactive-session",
			EntryAgent: agent.ID,
			Agents:     []agentflow.AgentSpec{agent},
		}, agent
	}

	bytes, err := os.ReadFile(workflowPath)
	if err != nil {
		fail(err)
	}
	var spec agentflow.WorkflowSpec
	if err := json.Unmarshal(bytes, &spec); err != nil {
		fail(err)
	}
	spec.SkillRoots = append(defaultSkillRoots(workdir), normalizeSkillRoots(spec.SkillRoots, filepath.Dir(workflowPath))...)
	if len(skillDirs) > 0 {
		spec.SkillRoots = append(spec.SkillRoots, normalizeSkillRoots(skillDirs, workdir)...)
	}
	spec.SkillRoots = dedupeStrings(spec.SkillRoots)
	targetID := requestedAgentID
	if targetID == "" {
		targetID = spec.EntryAgent
	}
	agent, ok := spec.Agent(targetID)
	if !ok {
		fail(fmt.Errorf("agent %s not found in workflow", targetID))
	}
	if len(agent.Capabilities.Tools) == 0 {
		agent.Capabilities.Tools = tools
	}
	agent.Skills = append(agent.Skills, skillRefs...)
	agent.Skills = dedupeStrings(agent.Skills)
	if len(agent.Capabilities.ReadPaths) == 0 {
		agent.Capabilities.ReadPaths = []string{workdir}
	}
	if len(agent.Capabilities.WritePaths) == 0 {
		agent.Capabilities.WritePaths = []string{workdir}
	}
	if len(agent.Mounts) == 0 {
		agent.Mounts = []capability.Mount{{Name: "workspace", HostPath: workdir}}
	}
	return spec, agent
}

type sliceFlags []string

func (s *sliceFlags) String() string {
	return strings.Join(*s, ",")
}

func (s *sliceFlags) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func normalizeSkillRoots(roots []string, baseDir string) []string {
	out := make([]string, 0, len(roots))
	seen := map[string]bool{}
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if !filepath.IsAbs(root) {
			root = filepath.Join(baseDir, root)
		}
		root = filepath.Clean(root)
		if seen[root] {
			continue
		}
		seen[root] = true
		out = append(out, root)
	}
	return out
}

func defaultSkillRoots(workdir string) []string {
	return dedupeStrings([]string{
		filepath.Join(workdir, "skills"),
		filepath.Join(filepath.Dir(workdir), "aos", "skills"),
	})
}

func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func parseTools(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func agentMounts(agent agentflow.AgentSpec, workdir string) []capability.Mount {
	if len(agent.Mounts) > 0 {
		return agent.Mounts
	}
	return []capability.Mount{{Name: "workspace", HostPath: workdir}}
}

func agentCapabilities(agent agentflow.AgentSpec, workdir string) capability.Set {
	set := agent.Capabilities
	if len(set.ReadPaths) == 0 {
		set.ReadPaths = []string{workdir}
	}
	if len(set.WritePaths) == 0 {
		set.WritePaths = []string{workdir}
	}
	return set
}

func maxSteps(agent agentflow.AgentSpec) int {
	if agent.MaxSteps > 0 {
		return agent.MaxSteps
	}
	return 12
}

func printResult(ui render.Palette, footer *render.FooterBar, result runtime.RunResult) {
	if result.Yielded && len(result.Session.Messages) > 0 {
		last := result.Session.Messages[len(result.Session.Messages)-1]
		if last.Role == "assistant" {
			footer.Println(ui.AgentBlock("agent> ", last.Content))
			return
		}
	}
	if result.Blocked {
		footer.Println(ui.Warn("agent> waiting for more input"))
		return
	}
	if result.ExitStatus.Reason != "" {
		style := ui.Subtle
		if result.ExitStatus.Code != 0 {
			style = ui.Error
		}
		footer.Println(style(fmt.Sprintf("agent exited: %s (code=%d retryable=%t)", result.ExitStatus.Reason, result.ExitStatus.Code, result.ExitStatus.Retryable)))
	}
}

func detectRepoRoot(workdir string) string {
	candidate := workdir
	for {
		if _, err := os.Stat(filepath.Join(candidate, "go.mod")); err == nil {
			return candidate
		}
		next := filepath.Dir(candidate)
		if next == candidate {
			return workdir
		}
		candidate = next
	}
}

// interactiveRunOptions prints the interactive agent's activity; observe
// mirrors it into the workflow progress row.
func interactiveRunOptions(ui render.Palette, stream *render.StreamRenderer, footer *render.FooterBar, observe func(kind, detail string)) runtime.RunOptions {
	stream.SetFooter(footer)
	return runtime.RunOptions{
		StopOnAssistantMessage: true,
		ContinueOnToolError:    true,
		OnAssistantMessage: func(message string) {
			stream.Clear()
			observe("message", message)
		},
		OnToolCall: func(call runtime.ToolCall) {
			stream.PrintStatus(ui.ToolCall("tool> " + render.SummarizeToolCall(call)))
			observe("tool_call", render.SummarizeToolCall(call))
		},
		OnToolResult: func(result tools.Result) {
			stream.PrintStatus(ui.ToolResult("tool< " + render.SummarizeToolResult(result)))
			observe("tool_result", "")
		},
		OnToolError: func(call runtime.ToolCall, err error) {
			stream.PrintStatus(ui.ToolError("tool! " + call.Name + ": " + err.Error()))
			observe("tool_error", err.Error())
		},
	}
}

func getenv(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func getenvDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value + "s")
	if err != nil || duration <= 0 {
		return fallback
	}
	return duration
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
