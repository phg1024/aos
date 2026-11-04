package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/inference"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/runlog"
	"aos/internal/runtime"
	"aos/internal/skills"
	"aos/internal/statedir"
	"aos/internal/tokenizer"
	"aos/internal/tools"
	"aos/internal/tui/render"
)

func main() {
	// The TUI drops API keys from its environment once it has read them;
	// switching sessions starts it again with the environment it had.
	launchEnv := os.Environ()
	if args := runTUI(); args != nil {
		if err := restart(args, launchEnv); err != nil {
			fail(err)
		}
	}
}

// runTUI runs the TUI until the user quits. It returns the arguments to
// start it again with when /resume switches to another session, after its
// deferred cleanup has released the terminal and the state directory.
func runTUI() []string {
	var workflowPath string
	var agentID string
	var systemPrompt string
	var stateDir string
	var sessionsDir string
	var workdir string
	var toolList string
	var skillDirs sliceFlags
	var skillRefs sliceFlags
	var modelsPath string

	flag.StringVar(&workflowPath, "workflow", "", "optional workflow JSON path")
	flag.StringVar(&agentID, "agent", "", "agent ID when using a workflow")
	flag.StringVar(&systemPrompt, "system-prompt", "You are a helpful AOS agent in an interactive terminal. Respond conversationally, inspect the workspace with list_dir or find_files before assuming file paths, use tools when useful, keep internal exploration in tool actions instead of visible chatter, use wait when you need more user input, and when a task is done, send one final user-facing message that summarizes the outcome: that ends your turn, and the user's next message continues the conversation.", "system prompt for direct interactive mode")
	flag.StringVar(&stateDir, "state-dir", "", "optional persistent state directory")
	flag.StringVar(&sessionsDir, "sessions-dir", os.Getenv("AOS_TUI_SESSIONS_DIR"), "directory of saved sessions for /resume (default: the state directory's parent)")
	flag.StringVar(&workdir, "workdir", ".", "workspace directory for read/write tools")
	flag.StringVar(&toolList, "tools", "list_dir,find_files,read_file,write_file,run,web_search,web_fetch", "comma-separated tools for direct interactive mode")
	flag.Var(&skillDirs, "skill-dir", "directory containing Claude-Code-style skills (repeatable)")
	flag.Var(&skillRefs, "skill", "skill name or skill path to load (repeatable)")
	flag.StringVar(&modelsPath, "models", getenv(inference.ProfilesFileEnv, inference.DefaultProfilesFile()), "model profiles file for /model")
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
	if sessionsDir == "" {
		sessionsDir = filepath.Dir(filepath.Clean(stateDir))
	}
	// One run per state directory, as for aos run; aos reap relies on it to
	// tell a live session's processes from orphans.
	releaseState, err := statedir.Lock(stateDir)
	if err != nil {
		fail(err)
	}
	defer releaseState()

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
	leaseManager, err := lease.New(filepath.Join(stateDir, "leases"), k)
	if err != nil {
		fail(err)
	}
	bus, err := ipc.New(filepath.Join(stateDir, "bus"), k)
	if err != nil {
		fail(err)
	}

	// A state directory that already holds this agent's session continues
	// it; otherwise a new interactive process starts.
	spawnRoot := func() (kernel.Process, error) {
		return k.Spawn(kernel.ProcessSpec{
			Role:          "agent:" + agent.ID,
			Metadata:      map[string]string{"agent_id": agent.ID, "goal": "Interactive terminal session", "workdir": absWorkdir},
			Mounts:        agentMounts(agent, absWorkdir),
			CapabilitySet: agentCapabilities(agent, absWorkdir),
			Delegable:     agent.Delegable,
		})
	}
	var root tuiRoot
	var resumeNote string
	var resumed bool
	root.process, root.checkpointID, root.session, root.seed, resumeNote, resumed = resumePoint(k, stateDir, agent.ID)
	if root.process.PID == "" {
		if root.process, err = spawnRoot(); err != nil {
			fail(err)
		}
	}

	// The environment's backend is one profile among those /model offers;
	// a session that switched before goes back to the model it chose.
	envConfig := inference.LoadBackendConfig(defaultModelTimeout)
	models, err := loadModels(getenv("AOS_MODEL_PROFILE", "env"), envConfig, modelsPath)
	if err != nil {
		fail(err)
	}
	scrubKeys(models.keyVariables())
	envProfile, _ := models.find(models.current)
	active, err := models.configure(envProfile)
	if err != nil {
		fail(err)
	}
	var modelNote string
	if events, err := k.Events(); err == nil {
		if chosen := lastSwitched(events); chosen != "" && chosen != models.current {
			profile, ok := models.find(chosen)
			planned, planErr := models.configure(profile)
			switch {
			case !ok:
				modelNote = fmt.Sprintf("this session last used model profile %s, which is not in %s; using %s", chosen, modelsPath, models.current)
			case planErr != nil:
				modelNote = fmt.Sprintf("this session last used model profile %s, but %v; using %s", chosen, planErr, models.current)
			default:
				active, models.current = planned, chosen
				modelNote = "model profile " + chosen + ", as this session last chose with /model"
			}
		}
	}
	llmConfig := active.config
	modelName := llmConfig.Model
	repoRoot := detectRepoRoot(absWorkdir)
	stream := render.NewStreamRenderer(ui)

	// Model calls and delegated agents' trace go to disk as they happen, so
	// the session can be inspected later with aos inspect.
	callLog, err := runlog.Open(runlog.Path(stateDir, runlog.CallsFile))
	if err != nil {
		fail(err)
	}
	newBackend := func(config inference.BackendConfig) *inference.VLLMBackend {
		backend := inference.NewBackendFromConfig(config, inference.BackendOptions{OnStreamEvent: stream.Handle})
		backend.SetCallLog(callLog)
		return backend
	}
	completer := &swapCompleter{backend: newBackend(llmConfig)}
	traceLog, err := runlog.Open(runlog.Path(stateDir, runlog.TraceFile))
	if err != nil {
		fail(err)
	}

	// No tool reaches the state directory or another saved session;
	// web_fetch refuses the model endpoint unless allowlisted.
	toolRuntime := tools.NewRuntime()
	toolRuntime.Protected = protectedPaths(stateDir, sessionsDir, absWorkdir)
	toolRuntime.ModelHosts = models.modelHosts()

	compaction := active.compaction
	agentRuntime := &runtime.AgentRuntime{
		Kernel:     k,
		Artifacts:  store,
		Tools:      toolRuntime,
		Bus:        bus,
		Leases:     leaseManager,
		MaxSteps:   maxSteps(agent),
		Compaction: compaction,
		// A new root moves on what an ended one left: see continueEnded.
		KeepMailAtExit: true,
	}
	loadedSkills, err := skills.NewRegistry(spec.SkillRoots).LoadMany(agent.Skills)
	if err != nil {
		fail(err)
	}
	agentRuntime.Backend = agentflow.PromptBackend{
		Completer: completer,
		Workflow:  spec,
		Agent:     agent,
		Skills:    loadedSkills,
	}

	footer := render.NewFooterBar(ui)
	defer footer.Unpin()
	observeRoot := func(kind, detail string) {}
	var engine *agentflow.Engine
	if workflowPath != "" {
		progress := render.NewWorkflowProgress(ui, spec)
		footer.SetProgress(progress)
		engine = &agentflow.Engine{
			Kernel:    k,
			Artifacts: store,
			Bus:       bus,
			Leases:    leaseManager,
			Tools:     agentRuntime.Tools,
			Factory: agentflow.StaticBackendFactory{
				Completer: completer,
				Skills:    skills.NewRegistry(spec.SkillRoots),
			},
			Compaction: compaction,
			Trace: agentflow.Tracers(agentflow.TraceToLog(traceLog), func(event agentflow.TraceEvent) {
				if line := progress.Apply(event); line != "" {
					stream.PrintStatus(line)
					return
				}
				footer.Draw()
			}),
		}
		agentRuntime.DelegateHandler = engine.DelegateHandler(spec, agentflow.RunRequest{Goal: root.process.Metadata["goal"]}, agent, nil)
		observeRoot = func(kind, detail string) {
			progress.Observe(agentflow.TraceEvent{Time: time.Now(), AgentID: agent.ID, PID: root.process.PID, Kind: kind, Detail: detail})
			footer.Draw()
		}
		stopTicker := startProgressTicker(progress, footer)
		defer stopTicker()
	}
	var tokenCounter tokenizer.Counter
	startTokenizer := func() {
		if tokenCounter != nil {
			tokenCounter.Close()
			tokenCounter = nil
		}
		if sidecar, sidecarErr := tokenizer.Start(getenv("AOS_TOKENIZER_PYTHON", "python3"), repoRoot, modelName); sidecarErr == nil {
			tokenCounter = sidecar
		}
	}
	startTokenizer()
	defer func() {
		if tokenCounter != nil {
			tokenCounter.Close()
		}
	}()

	fmt.Println(ui.Banner(render.AOSBanner()))
	fmt.Println(ui.Header(fmt.Sprintf("AOS TUI ready  agent=%s  pid=%s", agent.ID, root.process.PID)))
	fmt.Println(ui.Subtle("Backend: " + string(llmConfig.Kind)))
	fmt.Println(ui.Subtle(fmt.Sprintf("Model: %s  (profile %s; /model switches)", modelName, models.current)))
	if modelNote != "" {
		fmt.Println(ui.Subtle(modelNote))
	}
	if tokenCounter != nil {
		fmt.Println(ui.Subtle("Tokenizer: exact sidecar"))
	} else {
		fmt.Println(ui.Subtle("Tokenizer: heuristic fallback"))
	}
	fmt.Println(ui.Subtle("State: " + stateDir))
	fmt.Println(ui.Subtle("Type / for commands (Tab completes, /help lists them)  ·  Esc cancels a running task"))

	if workflowPath != "" {
		fmt.Println(ui.Subtle(fmt.Sprintf("Workflow: %s  entry=%s  delegates=%s", spec.Name, agent.ID, strings.Join(agent.AllowedAgents, ","))))
	}

	if resumed {
		fmt.Println(ui.Header(fmt.Sprintf("Resumed session %s", filepath.Base(stateDir))))
		if resumeNote != "" {
			fmt.Println(ui.Subtle(resumeNote))
		}
		// The whole conversation, including what compaction archived.
		for _, turn := range conversation(withArchive(store, root.session)) {
			if turn.user {
				fmt.Println(ui.Prompt("you> ") + turn.text)
			} else {
				fmt.Println(ui.AgentReply("agent> ", turn.text))
			}
		}
	}

	// interrupted marks that Esc stopped the last run, so the next message
	// tells the agent its previous task was cut short.
	var interrupted bool
	editor := render.NewLineEditor(ui, os.Stdin, tuiCommands)
	useEditor := true
	scanner := bufio.NewScanner(os.Stdin)
	for {
		footer.Update(render.FooterStatus{
			Process:      root.process,
			Model:        modelName,
			CheckpointID: root.checkpointID,
			Session:      root.session,
			Counter:      tokenCounter,
			Backend:      agentRuntime.Backend.(agentflow.PromptBackend),
		})
		footer.Draw()
		var line string
		if useEditor {
			typed, err := editor.ReadLine(ui.Prompt("you> "))
			switch {
			case errors.Is(err, render.ErrLineEditorUnavailable):
				useEditor = false
			case err != nil:
				return nil
			default:
				line = typed
			}
		}
		if !useEditor {
			fmt.Print(ui.Prompt("you> "))
			if !scanner.Scan() {
				fmt.Println()
				return nil
			}
			line = scanner.Text()
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		command := ""
		switch {
		case strings.HasPrefix(line, "//"):
			// A doubled slash sends the line to the agent with one slash.
			line = line[1:]
		case strings.HasPrefix(line, "/"):
			known, ok := render.LookupCommand(tuiCommands, line)
			if !ok {
				footer.Println(ui.Warn(unknownCommandNotice(line)))
				continue
			}
			command = known.Name
		}
		switch command {
		case "/quit":
			return nil
		case "/help":
			footer.Println(helpText(ui))
			continue
		case "/resume":
			sessions, err := listSessions(sessionsDir)
			if err != nil {
				footer.Println(ui.Error("resume: " + err.Error()))
				continue
			}
			// This session is where the user already is; it is not offered.
			sessions = otherSessions(sessions, stateDir)
			arg := strings.TrimSpace(strings.TrimPrefix(line, "/resume"))
			var picked sessionInfo
			if arg == "" {
				if len(sessions) == 0 || !useEditor {
					footer.Println(sessionList(ui, sessions, sessionsDir))
					continue
				}
				rows := make([]string, len(sessions))
				for i, session := range sessions {
					rows[i] = sessionRow(ui, i, session)
				}
				chosen, err := render.Pick(ui, os.Stdin, "Resume", rows)
				switch {
				case errors.Is(err, render.ErrLineEditorUnavailable):
					footer.Println(sessionList(ui, sessions, sessionsDir))
					continue
				case err != nil || chosen < 0:
					footer.Println(ui.Subtle("resume cancelled"))
					continue
				}
				picked = sessions[chosen]
			} else if picked, err = pickSession(sessions, arg); err != nil {
				footer.Println(ui.Warn(err.Error()))
				continue
			}
			switch {
			case picked.InUse:
				footer.Println(ui.Warn("that session is open in another TUI"))
			default:
				footer.Println(ui.Subtle("switching to session " + picked.Name + " …"))
				return restartArgs(os.Args[1:], picked.Dir, sessionsDir)
			}
			continue
		case "/clear":
			fmt.Print("\x1b[2J\x1b[H")
			continue
		case "/state":
			footer.Println(ui.Subtle(fmt.Sprintf("pid=%s checkpoint=%s state_dir=%s model=%s (%s, %s)", root.process.PID, root.checkpointID, stateDir, models.current, modelName, llmConfig.BaseURL)))
			continue
		case "/model":
			arg := strings.TrimSpace(strings.TrimPrefix(line, "/model"))
			var profile inference.Profile
			if arg == "" {
				if !useEditor {
					footer.Println(modelList(ui, models, modelsPath))
					continue
				}
				rows := make([]string, len(models.profiles))
				for i, candidate := range models.profiles {
					rows[i] = modelRow(ui, i, candidate, models.current)
				}
				chosen, err := render.Pick(ui, os.Stdin, "Model", rows)
				switch {
				case errors.Is(err, render.ErrLineEditorUnavailable):
					footer.Println(modelList(ui, models, modelsPath))
					continue
				case err != nil || chosen < 0:
					footer.Println(ui.Subtle("model unchanged"))
					continue
				}
				profile = models.profiles[chosen]
			} else {
				var ok bool
				if profile, ok = models.find(arg); !ok {
					footer.Println(ui.Warn("no model profile " + arg + " · /model lists them"))
					continue
				}
			}
			if profile.Name == models.current {
				footer.Println(ui.Subtle("already on " + profile.Name))
				continue
			}
			planned, err := models.planSwitch(profile, root.session)
			if err != nil {
				footer.Println(ui.Warn("model unchanged: " + err.Error()))
				continue
			}
			// The event comes first: if it cannot be recorded, nothing changes.
			if _, err := k.RecordEvent(root.process.PID, modelSwitchedEvent, switchedData(models.current, planned)); err != nil {
				footer.Println(ui.Error("model unchanged: " + err.Error()))
				continue
			}
			completer.set(newBackend(planned.config))
			agentRuntime.Compaction = planned.compaction
			if engine != nil {
				engine.Compaction = planned.compaction
			}
			models.current, llmConfig, modelName = profile.Name, planned.config, planned.config.Model
			startTokenizer()
			footer.Println(ui.Subtle(fmt.Sprintf("model: %s (%s at %s) from the next model call; history, memory and budgets carry over", profile.Name, modelName, llmConfig.BaseURL)))
			if planned.notice != "" {
				footer.Println(ui.Warn(planned.notice))
			}
			continue
		case "/recover":
			if root.checkpointID == "" {
				footer.Println(ui.Warn("no checkpoint yet"))
				continue
			}
			result, err := runCancellable(ui, stream, func(ctx context.Context) (runtime.RunResult, error) {
				return agentRuntime.Recover(ctx, root.process.PID, root.checkpointID, nil, interactiveRunOptions(ui, stream, footer, observeRoot))
			})
			if errors.Is(err, runtime.ErrRunCancelled) {
				interrupted = true
				footer.Println(ui.Warn(cancelledNotice))
			} else if err != nil && err != runtime.ErrAgentBlocked {
				stream.Clear()
				footer.Println(ui.Error("recover error: " + err.Error()))
				continue
			}
			root.record(store, result)
			stream.Clear()
			printResult(ui, footer, result)
			continue
		}

		message := line
		wasInterrupted := interrupted
		if interrupted {
			message = interruptedPrefix + line
			interrupted = false
		}
		// An agent whose exit is final cannot be recovered; a new one takes
		// over its conversation, and the line goes to it.
		if note, err := continueEnded(k, store, bus, &root, spawnRoot); err != nil {
			footer.Println(ui.Error("not delivered: " + err.Error()))
			interrupted = wasInterrupted
			continue
		} else if note != "" {
			footer.Println(ui.Subtle(note))
		}
		// The line reaches the agent through its mailbox. An agent that
		// exited cannot receive mail; resuming it recovers it, and the line
		// goes with the recovery instead.
		appended, err := deliver(bus, &root, message)
		if err != nil {
			footer.Println(ui.Error("not delivered: " + err.Error()))
			interrupted = wasInterrupted
			continue
		}

		observeRoot("start", line)
		// Each message starts a turn with a fresh step allowance; tokens and
		// time keep counting against the session's budget.
		turnOptions := interactiveRunOptions(ui, stream, footer, observeRoot)
		turnOptions.NewTurn = true
		before := root.checkpointID
		result, err := runCancellable(ui, stream, func(ctx context.Context) (runtime.RunResult, error) {
			return runTurn(ctx, agentRuntime, &root, appended, turnOptions)
		})
		if errors.Is(err, runtime.ErrRunCancelled) {
			interrupted = true
			stream.Clear()
			footer.Println(ui.Warn(cancelledNotice))
		} else if err != nil && err != runtime.ErrAgentBlocked {
			stream.Clear()
			footer.Println(ui.Error("runtime error: " + err.Error()))
		}
		observeRoot("exit", "")
		if lostLine(appended, before, result) {
			footer.Println(ui.Warn("your message was not delivered; send it again"))
			interrupted = interrupted || wasInterrupted
		}
		root.record(store, result)
		stream.Clear()
		printResult(ui, footer, result)
	}
}

// tuiCommands are the slash commands the input line highlights and completes.
var tuiCommands = []render.Command{
	{Name: "/help", Summary: "list commands and keys"},
	{Name: "/state", Summary: "show the process, checkpoint and state directory"},
	{Name: "/resume", Summary: "pick a saved session with ↑/↓ and Enter; /resume N switches to one"},
	{Name: "/recover", Summary: "restart the agent from its last checkpoint"},
	{Name: "/model", Summary: "pick a model profile with ↑/↓ and Enter; /model NAME switches to one"},
	{Name: "/clear", Summary: "clear the screen"},
	{Name: "/quit", Summary: "exit"},
}

// otherSessions is sessions without the one in stateDir.
func otherSessions(sessions []sessionInfo, stateDir string) []sessionInfo {
	var others []sessionInfo
	for _, session := range sessions {
		if filepath.Clean(session.Dir) != filepath.Clean(stateDir) {
			others = append(others, session)
		}
	}
	return others
}

// sessionRow is the i-th saved session as the list and the picker show it.
func sessionRow(ui render.Palette, i int, session sessionInfo) string {
	preview := session.Preview
	if preview == "" {
		preview = "(nothing said yet)"
	}
	if runes := []rune(preview); len(runes) > 60 {
		preview = string(runes[:60]) + "…"
	}
	status := ""
	if session.InUse {
		status = "  " + ui.Warn("in use")
	}
	turns := fmt.Sprintf("%d turns", session.Turns)
	if session.Turns == 1 {
		turns = "1 turn"
	}
	return fmt.Sprintf("%2d  %-28s %-12s %9s  %s%s", i+1, session.Name, ago(session.Updated), turns, ui.Subtle(preview), status)
}

// sessionList renders the saved sessions, numbered for /resume N, where
// the picker is not available.
func sessionList(ui render.Palette, sessions []sessionInfo, dir string) string {
	if len(sessions) == 0 {
		return ui.Subtle("no other saved sessions in " + dir)
	}
	var builder strings.Builder
	builder.WriteString(ui.Header(" Sessions ") + ui.Subtle("  "+dir) + "\n")
	for i, session := range sessions {
		builder.WriteString("  " + sessionRow(ui, i, session) + "\n")
	}
	builder.WriteString(ui.Subtle("  /resume N switches to session N"))
	return builder.String()
}

func helpText(ui render.Palette) string {
	var builder strings.Builder
	builder.WriteString(ui.Header(" Commands ") + "\n")
	for _, command := range tuiCommands {
		builder.WriteString(fmt.Sprintf("  %s  %s\n", ui.CommandName(fmt.Sprintf("%-9s", command.Name)), command.Summary))
	}
	builder.WriteString(ui.Subtle("  Tab completes a command · ↑/↓ recall input · Esc cancels a running task · //text sends a line starting with /"))
	return builder.String()
}

func unknownCommandNotice(line string) string {
	word, _, _ := strings.Cut(line, " ")
	notice := "unknown command " + word + " · /help lists commands · start with // to send it to the agent"
	if matches := render.MatchCommands(tuiCommands, word[:min(len(word), 2)]); len(matches) > 0 && len(word) > 1 {
		names := make([]string, 0, len(matches))
		for _, match := range matches {
			names = append(names, match.Name)
		}
		notice += " · did you mean " + strings.Join(names, " ") + "?"
	}
	return notice
}

const (
	cancelledNotice   = "cancelled (esc). Your next message continues from here."
	interruptedPrefix = "[The user pressed Esc and interrupted your previous task before it finished. Do not resume it unless asked.]\n"
)

// runCancellable runs fn with a context that an Esc press cancels.
func runCancellable(ui render.Palette, stream *render.StreamRenderer, fn func(ctx context.Context) (runtime.RunResult, error)) (runtime.RunResult, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once
	stop := render.WatchEsc(os.Stdin, func() {
		once.Do(func() {
			stream.PrintStatus(ui.Warn("esc: cancelling…"))
			cancel()
		})
	})
	defer stop()
	return fn(ctx)
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
				// The web tools you list are what grants network; it then
				// also reaches run.
				AllowNetwork: slices.Contains(tools, "web_search") || slices.Contains(tools, "web_fetch"),
				Tools:        tools,
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
	if result.TurnExit != nil {
		footer.Println(ui.Subtle(fmt.Sprintf("agent ended its turn: %s (code=%d); your next message continues the conversation", result.TurnExit.Reason, result.TurnExit.Code)))
		return
	}
	if result.Yielded && len(result.Session.Messages) > 0 {
		last := result.Session.Messages[len(result.Session.Messages)-1]
		if last.Role == "assistant" {
			footer.Println(ui.AgentReply("agent> ", last.Content))
			return
		}
	}
	if result.Blocked {
		if text := runtime.WaitText(result.Session.Messages); text != "" {
			footer.Println(ui.AgentReply("agent> ", text))
		}
		footer.Println(ui.Subtle("(the agent is waiting for your reply)"))
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
	// An exit ends the turn, not the session: the next message continues
	// the same agent, with its archive and working memory.
	return runtime.RunOptions{
		StopOnAssistantMessage: true,
		ExitEndsTurn:           true,
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
