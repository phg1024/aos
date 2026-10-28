package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"aos/internal/agentflow"
	"aos/internal/app"
	"aos/internal/artifact"
	"aos/internal/inference"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/skills"
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
	default:
		printUsage()
	}
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
	fs.Var(&inputs, "input", "input file path (repeatable)")
	fs.StringVar(&outputPath, "output", "", "output file path")
	fs.StringVar(&workflowPath, "workflow", "", "workflow JSON spec path")
	fs.StringVar(&reportPath, "report", "", "optional HTML report output path")
	fs.StringVar(&stateDir, "state-dir", "", "optional state directory for workflow runs")
	fs.BoolVar(&trace, "trace", getenvBool("AOS_TRACE", false), "print real-time agent actions during workflow runs")
	fs.StringVar(&verify, "verify", string(workflow.VerificationNone), "verification policy without --workflow: none, required, or best_effort")
	_ = fs.Parse(args)

	goal := ""
	if fs.NArg() > 0 {
		goal = fs.Arg(0)
	}

	llmConfig := inference.LoadBackendConfig(120 * time.Second)
	backend := inference.NewBackendFromConfig(llmConfig, inference.BackendOptions{})

	if workflowPath != "" {
		if err := runWorkflow(context.Background(), backend, workflowPath, []string(inputs), outputPath, reportPath, stateDir, goal, trace); err != nil {
			fmt.Fprintln(os.Stderr, err)
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

func runWorkflow(ctx context.Context, backend agentflow.JSONCompleter, workflowPath string, inputs []string, outputPath, reportPath, stateDir, goal string, trace bool) error {
	specBytes, err := os.ReadFile(workflowPath)
	if err != nil {
		return err
	}
	var spec agentflow.WorkflowSpec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return err
	}
	spec.SkillRoots = dedupeStrings(append(defaultSkillRoots(filepath.Dir(workflowPath)), resolveSkillRoots(spec.SkillRoots, filepath.Dir(workflowPath))...))

	if stateDir == "" {
		stateDir, err = os.MkdirTemp("", "aos-workflow-*")
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	releaseLock, err := acquireStateLock(stateDir)
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
	leaseManager, err := lease.New(filepath.Join(stateDir, "leases"))
	if err != nil {
		return err
	}

	engine := agentflow.Engine{
		Kernel:    k,
		Artifacts: store,
		Leases:    leaseManager,
		Tools:     tools.NewRuntime(),
		Factory: agentflow.StaticBackendFactory{
			Completer: backend,
			Skills:    skills.NewRegistry(spec.SkillRoots),
		},
	}
	if trace {
		engine.Trace = agentflow.NewStdoutTracer()
	}

	summary, err := engine.Run(ctx, spec, agentflow.RunRequest{
		Goal:       goal,
		InputPaths: inputs,
		OutputPath: outputPath,
	})
	if err != nil {
		return err
	}

	if reportPath != "" {
		report, err := agentflow.CollectRunReport(k, store, "AOS Workflow Run", summary)
		if err != nil {
			return err
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
		if err := os.WriteFile(reportPath, []byte(agentflow.RenderHTML(report)), 0o644); err != nil {
			return err
		}
	}

	bytes, err := json.MarshalIndent(map[string]any{
		"workflow":      spec.Name,
		"root_pid":      summary.RootPID,
		"entry_pid":     summary.EntryPID,
		"result_id":     summary.ResultID,
		"output_path":   outputPath,
		"state_dir":     stateDir,
		"report_path":   reportPath,
		"agent_run_cnt": countAgentRuns(summary.AgentRuns),
	}, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(bytes))
	return nil
}

func acquireStateLock(stateDir string) (func(), error) {
	lockPath := filepath.Clean(stateDir) + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("state directory is already in use by another aos run: %s", stateDir)
		}
		return nil, err
	}
	if err := lockFile.Truncate(0); err == nil {
		_, _ = lockFile.WriteString(fmt.Sprintf("pid=%d\nstarted_at=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339)))
		_, _ = lockFile.Seek(0, 0)
	}
	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
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
