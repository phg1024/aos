package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aos/internal/capability"
	"aos/internal/tools"
	"aos/internal/workflow"
)

var ErrVerificationFailed = errors.New("verification failed")

type JSONCompleter interface {
	CompleteJSON(ctx context.Context, systemPrompt, userPrompt string, target any) error
}

type AssembleRequest struct {
	Goal       string
	InputPaths []string
	OutputPath string
	// Verification defaults to workflow.VerificationNone.
	Verification workflow.VerificationPolicy
	// Model names the completer's model, for the result's record of who
	// verified; empty when the caller does not know it.
	Model string
}

type AssembleResult struct {
	OutputPath         string
	Verification       workflow.VerificationPolicy
	Approved           bool
	VerificationReason string
	OutputBytes        int
	SourceCount        int
	Mode               string
	ScriptPath         string
	// WorkerModel and VerifierModel are the models of the two calls, and
	// VerifierSameAsWorker says the verifier was the worker's own completer:
	// a fresh call to the same model, not an independent judge. AssembleFiles
	// takes one completer, so it always is; the result says so.
	WorkerModel          string
	VerifierModel        string `json:",omitempty"`
	VerifierSameAsWorker bool
}

type workerResponse struct {
	Content string `json:"content"`
}

type proceduralWorkerResponse struct {
	Python string `json:"python"`
}

type verifierResponse struct {
	Approved bool   `json:"approved"`
	Reason   string `json:"reason"`
}

func AssembleFiles(ctx context.Context, completer JSONCompleter, req AssembleRequest) (AssembleResult, error) {
	if strings.TrimSpace(req.Goal) == "" {
		return AssembleResult{}, errors.New("goal is required")
	}
	if len(req.InputPaths) == 0 {
		return AssembleResult{}, errors.New("at least one input path is required")
	}
	if strings.TrimSpace(req.OutputPath) == "" {
		return AssembleResult{}, errors.New("output path is required")
	}
	policy, err := workflow.ParseVerificationPolicy(string(req.Verification))
	if err != nil {
		return AssembleResult{}, err
	}
	if policy == "" {
		policy = workflow.VerificationNone
	}
	req.Verification = policy
	if proceduralRequested(req.Goal) {
		return assembleFilesProcedural(ctx, completer, req)
	}
	return assembleFilesSynthesis(ctx, completer, req)
}

func assembleFilesSynthesis(ctx context.Context, completer JSONCompleter, req AssembleRequest) (AssembleResult, error) {
	toolRuntime := tools.NewRuntime()
	mounts, set := deriveCapabilities(req.InputPaths, req.OutputPath)
	var sourceSections []string
	for _, inputPath := range req.InputPaths {
		result, err := toolRuntime.Execute(tools.Request{
			Tool:          "read_file",
			Args:          map[string]string{"path": inputPath},
			Mounts:        mounts,
			CapabilitySet: set,
		})
		if err != nil {
			return AssembleResult{}, err
		}
		sourceSections = append(sourceSections, fmt.Sprintf("FILE: %s\n%s", inputPath, result.Output["content"]))
	}

	var workerOut workerResponse
	workerSystem := "Return only compact JSON matching {\"content\":\"...\"}. Consolidate the provided source files into one coherent output that satisfies the user's goal. Do not mention that you are an AI."
	workerUser := "Goal:\n" + req.Goal + "\n\nSources:\n" + strings.Join(sourceSections, "\n\n---\n\n")
	if err := completer.CompleteJSON(ctx, workerSystem, workerUser, &workerOut); err != nil {
		return AssembleResult{}, err
	}

	verifierSystem := "Return only compact JSON matching {\"approved\":true|false,\"reason\":\"...\"}. Approve only if the output satisfies the goal using the provided sources without obvious hallucination."
	verifierUser := "Goal:\n" + req.Goal + "\n\nSources:\n" + strings.Join(sourceSections, "\n\n---\n\n") +
		"\n\nCandidate output:\n" + workerOut.Content
	verifierOut, err := verifyOutput(ctx, completer, req.Verification, verifierSystem, verifierUser)
	if err != nil {
		return AssembleResult{}, err
	}

	if _, err := toolRuntime.Execute(tools.Request{
		Tool: "write_file",
		Args: map[string]string{
			"path":    req.OutputPath,
			"content": workerOut.Content,
		},
		Mounts:        mounts,
		CapabilitySet: set,
	}); err != nil {
		return AssembleResult{}, err
	}

	return withVerifier(AssembleResult{
		OutputPath:         req.OutputPath,
		Verification:       req.Verification,
		Approved:           verifierOut.Approved,
		VerificationReason: verifierOut.Reason,
		OutputBytes:        len(workerOut.Content),
		SourceCount:        len(req.InputPaths),
		Mode:               "synthesis",
	}, req), nil
}

func assembleFilesProcedural(ctx context.Context, completer JSONCompleter, req AssembleRequest) (AssembleResult, error) {
	system := "Return only compact JSON matching {\"python\":\"...\"}. " +
		"Write a complete Python 3 program using only the standard library. " +
		"The program will be executed as: python3 script.py <input1> <input2> ... --output <output>. " +
		"It must read the input files, combine them to satisfy the user's goal, and write the final output file."
	user := "Goal:\n" + req.Goal + "\n\n" +
		"Input files:\n" + strings.Join(req.InputPaths, "\n") + "\n\n" +
		"Output file:\n" + req.OutputPath + "\n\n" +
		"Use only Python standard library. Do not print explanations."

	var generated proceduralWorkerResponse
	if err := completer.CompleteJSON(ctx, system, user, &generated); err != nil {
		return AssembleResult{}, err
	}
	if strings.TrimSpace(generated.Python) == "" {
		return AssembleResult{}, errors.New("generated python script was empty")
	}

	scriptDir, err := os.MkdirTemp("", "aos-procedural-*")
	if err != nil {
		return AssembleResult{}, err
	}
	scriptPath := filepath.Join(scriptDir, "generated_task.py")
	if err := os.WriteFile(scriptPath, []byte(generated.Python), 0o755); err != nil {
		return AssembleResult{}, err
	}

	// The program is a model's, so it runs as run runs: confined to reading
	// the inputs and its own script, and writing the output's directory.
	mounts, set := deriveCapabilities(req.InputPaths, req.OutputPath)
	mounts = append(mounts, capability.Mount{Name: "script", HostPath: scriptDir, ReadOnly: true})
	set.ReadPaths = append(set.ReadPaths, scriptDir)
	words := []string{"python3", scriptPath}
	words = append(words, req.InputPaths...)
	words = append(words, "--output", req.OutputPath)
	for i, word := range words {
		words[i] = shellQuote(word)
	}
	ran, err := tools.NewRuntime().ExecuteContext(ctx, tools.Request{
		Tool:          "run",
		Args:          map[string]string{"command": strings.Join(words, " "), "timeout": "10m"},
		Mounts:        mounts,
		CapabilitySet: set,
		PID:           "assemble",
	})
	if err != nil {
		return AssembleResult{}, fmt.Errorf("generated python not run: %w", err)
	}
	if code := ran.Output["exit_code"]; code != "0" {
		return AssembleResult{}, fmt.Errorf("generated python failed: exit %s: %s", code, strings.TrimSpace(ran.Output["stdout"]+" "+ran.Output["stderr"]))
	}

	bytes, err := os.ReadFile(req.OutputPath)
	if err != nil {
		return AssembleResult{}, err
	}

	verifierSystem := "Return only compact JSON matching {\"approved\":true|false,\"reason\":\"...\"}. " +
		"Approve only if the produced file appears to satisfy the goal and is a coherent combined result."
	verifierUser := "Goal:\n" + req.Goal + "\n\n" +
		"Input files:\n" + strings.Join(req.InputPaths, "\n") + "\n\n" +
		"Generated output:\n" + string(bytes)
	verifierOut, err := verifyOutput(ctx, completer, req.Verification, verifierSystem, verifierUser)
	if err != nil {
		if errors.Is(err, ErrVerificationFailed) {
			_ = os.Remove(req.OutputPath)
		}
		return AssembleResult{}, err
	}

	return withVerifier(AssembleResult{
		OutputPath:         req.OutputPath,
		Verification:       req.Verification,
		Approved:           verifierOut.Approved,
		VerificationReason: verifierOut.Reason,
		OutputBytes:        len(bytes),
		SourceCount:        len(req.InputPaths),
		Mode:               "procedural",
		ScriptPath:         scriptPath,
	}, req), nil
}

// withVerifier records who did the work and who verified it.
func withVerifier(result AssembleResult, req AssembleRequest) AssembleResult {
	result.WorkerModel = req.Model
	if req.Verification != workflow.VerificationNone {
		result.VerifierModel = req.Model
		result.VerifierSameAsWorker = true
	}
	return result
}

// shellQuote quotes word for sh.
func shellQuote(word string) string {
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// BestEffortRejected reports whether result is a best_effort verification
// whose verifier judged the output and rejected it: the output was kept,
// and aos run exits non-zero so the rejection is not read as a pass. A
// verifier call that failed judged nothing.
func BestEffortRejected(result AssembleResult) bool {
	return result.Verification == workflow.VerificationBestEffort && !result.Approved && !strings.HasPrefix(result.VerificationReason, "verification error")
}

// verifyOutput asks the completer to review a candidate under policy. Only
// VerificationRequired turns a rejection or verifier error into an error.
func verifyOutput(ctx context.Context, completer JSONCompleter, policy workflow.VerificationPolicy, systemPrompt, userPrompt string) (verifierResponse, error) {
	if policy == workflow.VerificationNone {
		return verifierResponse{}, nil
	}
	var out verifierResponse
	if err := completer.CompleteJSON(ctx, systemPrompt, userPrompt, &out); err != nil {
		if policy == workflow.VerificationBestEffort {
			return verifierResponse{Reason: "verification error: " + err.Error()}, nil
		}
		return verifierResponse{}, err
	}
	if !out.Approved && policy == workflow.VerificationRequired {
		return verifierResponse{}, fmt.Errorf("%w: %s", ErrVerificationFailed, out.Reason)
	}
	return out, nil
}

func deriveCapabilities(inputPaths []string, outputPath string) ([]capability.Mount, capability.Set) {
	mountIndex := map[string]int{}
	var mounts []capability.Mount
	var readPaths []string
	var writePaths []string

	for idx, inputPath := range inputPaths {
		dir := filepath.Clean(filepath.Dir(inputPath))
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

	outputDir := filepath.Clean(filepath.Dir(outputPath))
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

	return mounts, capability.Set{
		ReadPaths:  uniqueStrings(readPaths),
		WritePaths: uniqueStrings(writePaths),
		Tools:      []string{"read_file", "write_file", "list_dir", "find_files", "run"},
	}
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

func proceduralRequested(goal string) bool {
	normalized := strings.ToLower(goal)
	return strings.Contains(normalized, "python") &&
		(strings.Contains(normalized, "run it") ||
			strings.Contains(normalized, "run the python") ||
			strings.Contains(normalized, "write a python program"))
}

type TimeoutConfig struct {
	Seconds int
}

func (t TimeoutConfig) Duration(defaultDuration time.Duration) time.Duration {
	if t.Seconds <= 0 {
		return defaultDuration
	}
	return time.Duration(t.Seconds) * time.Second
}
