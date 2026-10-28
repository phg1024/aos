package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aos/internal/agentflow"
	"aos/internal/app"
	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/inference"
	"aos/internal/inspect"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/runlog"
	"aos/internal/runtime"
	"aos/internal/skills"
	"aos/internal/statedir"
	"aos/internal/tools"
	"aos/internal/workflow"
)

type arrayFlags []string

func (a *arrayFlags) String() string {
	return fmt.Sprintf("%v", []string(*a))
}

func (a *arrayFlags) Set(value string) error {
	*a = append(*a, value)
	return nil
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		return
	}

	switch os.Args[1] {
	case "run":
		run(os.Args[2:])
	case "inspect":
		inspectRun(os.Args[2:])
	case "reap":
		reapRun(os.Args[2:])
	default:
		printUsage()
	}
}

// parseInterspersed parses flags wherever they appear among args and
// returns the other arguments in order, so "inspect DIR --json" works as
// well as "inspect --json DIR".
func parseInterspersed(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		_ = fs.Parse(args)
		if fs.NArg() == 0 {
			return positional
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// reapRun closes the processes a dead run left without an exit record.
func reapRun(args []string) {
	fs := flag.NewFlagSet("reap", flag.ExitOnError)
	positional := parseInterspersed(fs, args)
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: aos reap <state-dir>")
		os.Exit(2)
	}
	reaped, err := statedir.Reap(positional[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(reaped) == 0 {
		fmt.Println("nothing to reap: every process has an exit record")
		return
	}
	fmt.Printf("reaped %d orphaned processes (exit 1, retryable, %q): %s\n", len(reaped), statedir.OrphanReason, strings.Join(reaped, ", "))
}

// inspectRun prints what a run did from its state directory alone, with
// no model calls, so it also works after a crash or kill.
func inspectRun(args []string) {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	tail := fs.Int("tail", 20, "number of trailing trace events to show (-1 for all)")
	positional := parseInterspersed(fs, args)
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: aos inspect [--json] [--tail N] <state-dir>  (flags may also follow the path)")
		os.Exit(2)
	}
	report, err := inspect.Load(positional[0], *tail)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *asJSON {
		if err := report.WriteJSON(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	report.WriteText(os.Stdout)
}

func run(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var inputs arrayFlags
	var outputPath string
	var workflowPath string
	var reportPath string
	var stateDir string
	var trace bool
	var verify string
	var timeBudget time.Duration
	var strictTime bool
	var tokenBudget int
	fs.Var(&inputs, "input", "input file path (repeatable)")
	fs.StringVar(&outputPath, "output", "", "output file path")
	fs.StringVar(&workflowPath, "workflow", "", "workflow JSON spec path")
	fs.StringVar(&reportPath, "report", "", "optional HTML report output path")
	fs.StringVar(&stateDir, "state-dir", "", "optional state directory for workflow runs")
	fs.BoolVar(&trace, "trace", getenvBool("AOS_TRACE", false), "print real-time agent actions during workflow runs")
	fs.StringVar(&verify, "verify", string(workflow.VerificationNone), "verification policy without --workflow: none, required, or best_effort")
	fs.DurationVar(&timeBudget, "time-budget", 0, "wall-clock budget for a --workflow run (e.g. 15m); agents see the time used and left each step and are told to commit near the end")
	fs.BoolVar(&strictTime, "strict-time-budget", false, "enforce --time-budget: stop the run, cancelling the model call or command in flight, when it runs out")
	fs.IntVar(&tokenBudget, "token-budget", 0, "model tokens (prompt+completion, cached included) the whole --workflow run may use; enforced")
	_ = fs.Parse(args)

	goal := ""
	if fs.NArg() > 0 {
		goal = fs.Arg(0)
	}

	llmConfig := inference.LoadBackendConfig(120 * time.Second)
	backend := inference.NewBackendFromConfig(llmConfig, inference.BackendOptions{})

	if workflowPath != "" {
		budget := capability.Budget{Seconds: int(timeBudget.Round(time.Second).Seconds()), Tokens: tokenBudget}
		budget.StrictTime = strictTime && budget.Seconds > 0
		if err := runWorkflow(context.Background(), backend, llmConfig.CompactionPolicy(), budget, workflowPath, []string(inputs), outputPath, reportPath, stateDir, goal, trace); err != nil {
			fmt.Fprintln(os.Stderr, err)
			var failed workflowFailed
			if errors.As(err, &failed) {
				os.Exit(failed.exitCode())
			}
			os.Exit(1)
		}
		return
	}

	policy, err := workflow.ParseVerificationPolicy(verify)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	result, err := app.AssembleFiles(context.Background(), backend, app.AssembleRequest{
		Goal:         goal,
		InputPaths:   []string(inputs),
		OutputPath:   outputPath,
		Verification: policy,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	bytes, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(bytes))
}

func runWorkflow(ctx context.Context, backend agentflow.JSONCompleter, compaction runtime.CompactionPolicy, budget capability.Budget, workflowPath string, inputs []string, outputPath, reportPath, stateDir, goal string, trace bool) error {
	specBytes, err := os.ReadFile(workflowPath)
	if err != nil {
		return err
	}
	var spec agentflow.WorkflowSpec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return err
	}
	spec.SkillRoots = dedupeStrings(append(defaultSkillRoots(filepath.Dir(workflowPath)), resolveSkillRoots(spec.SkillRoots, filepath.Dir(workflowPath))...))
	createWritableMounts(spec)

	if stateDir == "" {
		stateDir, err = os.MkdirTemp("", "aos-workflow-*")
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	releaseLock, err := statedir.Lock(stateDir)
	if err != nil {
		return err
	}
	defer releaseLock()

	k, err := kernel.New(filepath.Join(stateDir, "kernel"))
	if err != nil {
		return err
	}
	store, err := artifact.New(filepath.Join(stateDir, "artifacts"))
	if err != nil {
		return err
	}
	leaseManager, err := lease.New(filepath.Join(stateDir, "leases"), k)
	if err != nil {
		return err
	}
	// The same mailbox bus aos-tui uses, so agents can send, publish and
	// subscribe in batch runs too.
	bus, err := ipc.New(filepath.Join(stateDir, "bus"), k)
	if err != nil {
		return err
	}

	engine := agentflow.Engine{
		Kernel:    k,
		Artifacts: store,
		Leases:    leaseManager,
		Bus:       bus,
		Tools:     toolRuntime(stateDir),
		Factory: agentflow.StaticBackendFactory{
			Completer:  backend,
			Skills:     skills.NewRegistry(spec.SkillRoots),
			Compaction: compaction,
		},
		Compaction: compaction,
		Budget:     budget,
	}
	// The trace and the model-call ledger go to disk as the run happens, so
	// a killed run can still be inspected (aos inspect).
	traceLog, err := runlog.Open(runlog.Path(stateDir, runlog.TraceFile))
	if err != nil {
		return err
	}
	callLog, err := runlog.Open(runlog.Path(stateDir, runlog.CallsFile))
	if err != nil {
		return err
	}
	if recorder, ok := backend.(interface{ SetCallLog(*runlog.Log) }); ok {
		recorder.SetCallLog(callLog)
	}
	var stdoutTrace func(agentflow.TraceEvent)
	if trace {
		stdoutTrace = agentflow.NewStdoutTracer()
	}
	engine.Trace = agentflow.Tracers(agentflow.TraceToLog(traceLog), stdoutTrace)

	summary, runErr := engine.Run(ctx, spec, agentflow.RunRequest{
		Goal:       goal,
		InputPaths: inputs,
		OutputPath: outputPath,
	})
	if runErr != nil && summary.RootPID == "" {
		// Nothing ran: the spec was invalid or the root could not start.
		return runErr
	}

	if reportPath != "" {
		report, err := agentflow.CollectRunReport(k, store, "AOS Workflow Run", summary)
		if err != nil {
			return errors.Join(runErr, err)
		}
		report.Decisions = []string{
			"Executed the user-supplied workflow spec without imposing built-in planner or verifier semantics.",
			"Used the generic multi-step agent loop so agents could iterate through tool and delegate actions until completion.",
		}
		report.Issues = []string{
			"Any model-side action formatting issues would surface as runtime parse or malformed-output failures in the event log.",
		}
		report.Tests = []string{
			"Runtime, workflow, and workflow-spec tests were expected to pass before this live run.",
		}
		if runErr != nil {
			report.Issues = append([]string{"The run stopped on an error: " + runErr.Error()}, report.Issues...)
		}
		if err := os.WriteFile(reportPath, []byte(agentflow.RenderHTML(report)), 0o644); err != nil {
			return errors.Join(runErr, err)
		}
	}

	result := map[string]any{
		"workflow":      spec.Name,
		"root_pid":      summary.RootPID,
		"entry_pid":     summary.EntryPID,
		"result_id":     summary.ResultID,
		"output_path":   outputPath,
		"state_dir":     stateDir,
		"report_path":   reportPath,
		"agent_run_cnt": countAgentRuns(summary.AgentRuns),
		"exit_status":   summary.ExitStatus,
		"trace_path":    traceLog.Path(),
		"calls_path":    callLog.Path(),
	}
	if runErr != nil {
		result["error"] = runErr.Error()
	}
	if summary.Verification != nil {
		result["verification"] = summary.Verification
		result["verification_id"] = summary.VerificationID
	}
	if metered, ok := backend.(interface{ Usage() inference.TokenUsage }); ok {
		result["usage"] = metered.Usage()
	}
	bytes, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(bytes))
	if runErr != nil {
		return runErr
	}
	if summary.ExitStatus.Code != 0 {
		return workflowFailed{status: summary.ExitStatus}
	}
	return nil
}

// workflowFailed reports a workflow whose root exited non-zero, after its
// summary and report were written.
type workflowFailed struct {
	status kernel.ExitStatus
}

func (w workflowFailed) Error() string {
	return "workflow failed: " + w.status.Reason
}

// exitCode passes the root's code through when it is a usable process exit
// code, and uses 1 otherwise.
func (w workflowFailed) exitCode() int {
	if w.status.Code >= 1 && w.status.Code <= 125 {
		return w.status.Code
	}
	return 1
}

// createWritableMounts makes each writable mount directory that does not
// exist yet but whose parent does, such as a verifier's own area next to
// the workspace, so a confined run can work in it from the first step.
func createWritableMounts(spec agentflow.WorkflowSpec) {
	for _, agent := range spec.Agents {
		for _, mount := range agent.Mounts {
			if mount.ReadOnly || mount.HostPath == "" {
				continue
			}
			if _, err := os.Stat(mount.HostPath); errors.Is(err, os.ErrNotExist) {
				if _, err := os.Stat(filepath.Dir(mount.HostPath)); err == nil {
					_ = os.Mkdir(mount.HostPath, 0o755)
				}
			}
		}
	}
}

// toolRuntime is the tools runtime for a run on stateDir: no tool reaches
// the state directory or its lock, and web_fetch refuses the model
// endpoint unless a capability set allowlists it.
func toolRuntime(stateDir string) *tools.Runtime {
	runtime := tools.NewRuntime()
	runtime.Protected = []string{stateDir, statedir.LockPath(stateDir)}
	if parsed, err := url.Parse(inference.LoadBackendConfig(0).BaseURL); err == nil && parsed.Hostname() != "" {
		runtime.ModelHosts = []string{parsed.Hostname()}
	}
	return runtime
}

func resolveSkillRoots(roots []string, baseDir string) []string {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if !filepath.IsAbs(root) {
			root = filepath.Join(baseDir, root)
		}
		out = append(out, filepath.Clean(root))
	}
	return out
}

func defaultSkillRoots(baseDir string) []string {
	return dedupeStrings([]string{
		filepath.Join(baseDir, "skills"),
		filepath.Join(filepath.Dir(baseDir), "skills"),
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

func countAgentRuns(runs []agentflow.AgentRun) int {
	total := 0
	for _, run := range runs {
		total++
		total += countAgentRuns(run.Children)
	}
	return total
}

func printUsage() {
	fmt.Println("Usage:")
	fmt.Println("  aos run --input A --input B --input C --output D [--verify none|required|best_effort] \"assemble files A, B, C into D\"")
	fmt.Println("  aos run --workflow workflow.json --input A --output B --report run.html \"complete the task\"")
	fmt.Println("  aos inspect [--json] [--tail N] <state-dir>")
	fmt.Println("  aos reap <state-dir>   exit processes left running by a run that is gone, so they can be recovered")
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
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func getenvBool(key string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	case "":
		return fallback
	default:
		return fallback
	}
}
