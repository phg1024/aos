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
	"aos/internal/runtime"
	"aos/internal/tools"
)

func main() {
	var workflowPath string
	var agentID string
	var systemPrompt string
	var stateDir string
	var workdir string
	var toolList string

	flag.StringVar(&workflowPath, "workflow", "", "optional workflow JSON path")
	flag.StringVar(&agentID, "agent", "", "agent ID when using a workflow")
	flag.StringVar(&systemPrompt, "system-prompt", "You are a helpful AOS agent in an interactive terminal. Respond conversationally, inspect the workspace with list_dir or find_files before assuming file paths, use tools when useful, and use wait when you need more user input.", "system prompt for direct interactive mode")
	flag.StringVar(&stateDir, "state-dir", "", "optional persistent state directory")
	flag.StringVar(&workdir, "workdir", ".", "workspace directory for read/write tools")
	flag.StringVar(&toolList, "tools", "list_dir,find_files,read_file,write_file,run,execute_code,web_search,web_fetch", "comma-separated tools for direct interactive mode")
	flag.Parse()

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

	spec, agent := loadInteractiveSpec(workflowPath, agentID, systemPrompt, absWorkdir, parseTools(toolList))

	k, err := kernel.New(filepath.Join(stateDir, "kernel"))
	if err != nil {
		fail(err)
	}
	store, err := artifact.New(filepath.Join(stateDir, "artifacts"))
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

	timeout := getenvDuration("AOS_VLLM_TIMEOUT_SECONDS", 180*time.Second)
	var streamBuffer string
	backend := inference.NewVLLMBackend(inference.VLLMConfig{
		BaseURL: getenv("AOS_VLLM_BASE_URL", "http://lenovo-px.local:8001"),
		Model:   getenv("AOS_VLLM_MODEL", "Qwen/Qwen3.5-27B"),
		APIKey:  getenv("AOS_VLLM_API_KEY", "dummy"),
		Timeout: timeout,
		OnStream: func(s string) {
			if s == "\x00" {
				fmt.Print("\033[2K\r")
				streamBuffer = ""
				return
			}
			if len(streamBuffer) < 80 {
				s = strings.ReplaceAll(s, "\n", " ")
				s = strings.ReplaceAll(s, "\r", " ")
				streamBuffer += s
				if len(streamBuffer) > 80 {
					streamBuffer = streamBuffer[:80] + "..."
				}
				fmt.Printf("\033[2K\r\033[5m%s\033[0m", streamBuffer)
			}
		},
	})

	agentRuntime := &runtime.AgentRuntime{
		Kernel:    k,
		Artifacts: store,
		Tools:     tools.NewRuntime(),
		Bus:       bus,
		Backend: agentflow.PromptBackend{
			Completer: backend,
			Workflow:  spec,
			Agent:     agent,
		},
		MaxSteps: maxSteps(agent),
	}

	fmt.Printf("AOS TUI ready. agent=%s pid=%s state=%s\n", agent.ID, process.PID, stateDir)
	fmt.Println("Commands: /quit, /state, /recover")

	var checkpointID string
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("you> ")
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
			fmt.Printf("pid=%s checkpoint=%s state_dir=%s\n", process.PID, checkpointID, stateDir)
			continue
		case "/recover":
			if checkpointID == "" {
				fmt.Println("no checkpoint yet")
				continue
			}
			result, err := agentRuntime.Recover(context.Background(), process.PID, checkpointID, nil, runtime.RunOptions{StopOnAssistantMessage: true, ContinueOnToolError: true})
			if err != nil && err != runtime.ErrAgentBlocked {
				fmt.Println("recover error:", err)
				continue
			}
			if result.PID != "" {
				process.PID = result.PID
			}
			checkpointID = result.CheckpointID
			printResult(result)
			continue
		}

		if _, err := bus.Send("user", process.PID, line); err != nil {
			fmt.Println("send error:", err)
			continue
		}

		var result runtime.RunResult
		if checkpointID == "" {
			result, err = agentRuntime.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{StopOnAssistantMessage: false, ContinueOnToolError: true})
		} else {
			result, err = agentRuntime.Resume(context.Background(), process.PID, checkpointID, nil, runtime.RunOptions{StopOnAssistantMessage: false, ContinueOnToolError: true})
		}
		if err != nil && err != runtime.ErrAgentBlocked {
			fmt.Println("runtime error:", err)
		}
		checkpointID = result.CheckpointID
		printResult(result)
	}
}

func loadInteractiveSpec(workflowPath, requestedAgentID, systemPrompt, workdir string, tools []string) (agentflow.WorkflowSpec, agentflow.AgentSpec) {
	if workflowPath == "" {
		agent := agentflow.AgentSpec{
			ID:           "interactive",
			SystemPrompt: systemPrompt,
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

func printResult(result runtime.RunResult) {
	if result.Yielded && len(result.Session.Messages) > 0 {
		last := result.Session.Messages[len(result.Session.Messages)-1]
		if last.Role == "assistant" {
			fmt.Printf("agent> %s\n", last.Content)
			return
		}
	}
	if result.Blocked {
		fmt.Println("agent> waiting for more input")
		return
	}
	if result.ExitStatus.Reason != "" {
		fmt.Printf("agent exited: %s (code=%d retryable=%t)\n", result.ExitStatus.Reason, result.ExitStatus.Code, result.ExitStatus.Retryable)
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
