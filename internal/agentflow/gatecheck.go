package agentflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"

	"aos/internal/tools"
)

// CheckResult is one gate check as the gate ran it.
type CheckResult struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	// Uses is a SHA-256 of each script or binary the check names, as it was
	// when the check ran; "missing" for one that was not there.
	Uses         map[string]string `json:"uses"`
	ExitCode     int               `json:"exit_code"`
	Want         int               `json:"want"`
	OutputSHA256 string            `json:"output_sha256,omitempty"`
	Confinement  string            `json:"confinement,omitempty"`
	Err          string            `json:"error,omitempty"`
	Passed       bool              `json:"passed"`
}

// runChecks runs the spec's gate checks, in order, in the verifier's
// confinement made stricter: its mounts and path scopes, run as its only
// tool, no network and never unconfined. A model chooses nothing here: the
// command and what it uses come from the spec.
func (e *Engine) runChecks(ctx context.Context, spec WorkflowSpec, req RunRequest, rootPID string) []CheckResult {
	verifier, _ := spec.Agent(spec.Verification.Verifier)
	set := agentCapabilities(verifier, req)
	set.Tools = []string{"run"}
	set.AllowNetwork = false
	set.UnconfinedRun = false
	mounts := agentMounts(verifier, req)
	var results []CheckResult
	for _, check := range spec.Verification.Checks {
		result := CheckResult{Name: check.Name, Command: check.Command, Want: check.ExitCode, Uses: map[string]string{}, ExitCode: -1}
		missing := false
		for _, used := range check.Uses {
			if sum, err := usedSHA256(used); err != nil {
				result.Uses[used] = "missing"
				missing = true
			} else {
				result.Uses[used] = sum
			}
		}
		if missing {
			result.Err = "a script or binary it uses is missing"
			results = append(results, result)
			continue
		}
		args := map[string]string{"command": check.Command}
		if check.Timeout != "" {
			args["timeout"] = check.Timeout
		}
		out, err := e.Tools.ExecuteContext(ctx, tools.Request{Tool: "run", Args: args, Mounts: mounts, CapabilitySet: set, PID: rootPID + "-check"})
		if err != nil {
			result.Err = err.Error()
			results = append(results, result)
			continue
		}
		if code, err := strconv.Atoi(out.Output["exit_code"]); err == nil {
			result.ExitCode = code
		}
		sum := sha256.Sum256([]byte(out.Output["stdout"] + "\x00" + out.Output["stderr"]))
		result.OutputSHA256 = hex.EncodeToString(sum[:])
		result.Confinement = out.Output["confinement"]
		result.Passed = result.ExitCode == check.ExitCode
		results = append(results, result)
		e.emitTrace("", rootPID, "gate_check", fmt.Sprintf("name=%s exit=%d want=%d", check.Name, result.ExitCode, check.ExitCode))
	}
	return results
}

// usedSHA256 hashes a file, or a directory's whole tree.
func usedSHA256(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return fileSHA256(path)
	}
	hash := hashWork([]string{path}, nil)
	if hash.Err != "" {
		return "", fmt.Errorf("%s", hash.Err)
	}
	return hash.SHA256, nil
}

// failedCheck is the first check that did not pass, if any.
func failedCheck(results []CheckResult) (CheckResult, bool) {
	for _, result := range results {
		if !result.Passed {
			return result, true
		}
	}
	return CheckResult{}, false
}
