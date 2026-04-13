package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aos/internal/artifact"
	"aos/internal/capability"
)

var ErrToolDenied = errors.New("tool execution denied by capabilities")
var ErrPathDenied = errors.New("path access denied by capabilities")

type Request struct {
	Tool          string
	Args          map[string]string
	CapabilitySet capability.Set
	Mounts        []capability.Mount
}

type Result struct {
	Tool    string            `json:"tool"`
	Summary string            `json:"summary"`
	Output  map[string]string `json:"output,omitempty"`
}

type Runtime struct {
	evaluator capability.Evaluator
}

func NewRuntime() *Runtime {
	return &Runtime{evaluator: capability.Evaluator{}}
}

func (r *Runtime) Execute(req Request) (Result, error) {
	if !r.evaluator.ToolAllowed(req.Tool, req.CapabilitySet) {
		return Result{}, ErrToolDenied
	}

	switch req.Tool {
	case "echo":
		return Result{
			Tool:    req.Tool,
			Summary: "echoed message",
			Output: map[string]string{
				"message": req.Args["message"],
			},
		}, nil
	case "write_file":
		path := filepath.Clean(req.Args["path"])
		if !r.evaluator.CanWrite(path, req.Mounts, req.CapabilitySet) {
			return Result{}, ErrPathDenied
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(path, []byte(req.Args["content"]), 0o644); err != nil {
			return Result{}, err
		}
		return Result{
			Tool:    req.Tool,
			Summary: "wrote file",
			Output: map[string]string{
				"path": path,
			},
		}, nil
	case "read_file":
		path := filepath.Clean(req.Args["path"])
		if !r.evaluator.CanRead(path, req.Mounts, req.CapabilitySet) {
			return Result{}, ErrPathDenied
		}
		contentBytes, err := os.ReadFile(path)
		if err != nil {
			return Result{}, err
		}
		return Result{
			Tool:    req.Tool,
			Summary: "read file",
			Output: map[string]string{
				"path":    path,
				"content": string(contentBytes),
			},
		}, nil
	case "list_dir":
		path := filepath.Clean(req.Args["path"])
		if path == "." || path == "" {
			path = mountRoot(req.Mounts)
		}
		if !r.evaluator.CanRead(path, req.Mounts, req.CapabilitySet) {
			return Result{}, ErrPathDenied
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return Result{}, err
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() {
				name += "/"
			}
			names = append(names, name)
		}
		sort.Strings(names)
		return Result{
			Tool:    req.Tool,
			Summary: "listed directory",
			Output: map[string]string{
				"path":    path,
				"entries": strings.Join(names, "\n"),
			},
		}, nil
	case "find_files":
		root := filepath.Clean(req.Args["root"])
		if root == "." || root == "" {
			root = mountRoot(req.Mounts)
		}
		if !r.evaluator.CanRead(root, req.Mounts, req.CapabilitySet) {
			return Result{}, ErrPathDenied
		}
		pattern := strings.ToLower(strings.TrimSpace(req.Args["pattern"]))
		if pattern == "" {
			return Result{}, errors.New("find_files requires pattern")
		}
		var matches []string
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !r.evaluator.CanRead(path, req.Mounts, req.CapabilitySet) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			candidate := strings.ToLower(rel)
			if candidate == "." {
				return nil
			}
			if strings.Contains(candidate, pattern) {
				matches = append(matches, path)
			}
			return nil
		})
		if err != nil {
			return Result{}, err
		}
		sort.Strings(matches)
		return Result{
			Tool:    req.Tool,
			Summary: "found files",
			Output: map[string]string{
				"root":    root,
				"pattern": pattern,
				"matches": strings.Join(matches, "\n"),
			},
		}, nil
	case "execute_code":
		language := strings.TrimSpace(req.Args["language"])
		if language == "" {
			language = "python3"
		}
		code := req.Args["code"]
		if code == "" {
			return Result{}, errors.New("execute_code requires code")
		}
		timeoutStr := req.Args["timeout"]
		timeout := 30 * time.Second
		if timeoutStr != "" {
			if parsed, parseErr := time.ParseDuration(timeoutStr); parseErr == nil {
				timeout = parsed
			}
		}
		var interpreter string
		switch language {
		case "python", "python3":
			interpreter = "python3"
		default:
			return Result{}, errors.New("execute_code only supports python3")
		}
		cmd := exec.Command(interpreter, "-c", code)
		cmd.Dir = mountRoot(req.Mounts)
		cmd.Stdin = nil
		var stdoutBuf, stderrBuf bytes.Buffer
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		err := cmd.Run()
		exitCode := 0
		if err != nil {
			if ctx.Err() != nil {
				return Result{}, errors.New("code execution timed out")
			}
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
		}
		return Result{
			Tool:    req.Tool,
			Summary: fmt.Sprintf("executed %s code (exit: %d)", language, exitCode),
			Output: map[string]string{
				"language":  language,
				"stdout":    stdoutBuf.String(),
				"stderr":    stderrBuf.String(),
				"exit_code": fmt.Sprintf("%d", exitCode),
			},
		}, nil
	case "run":
		command := strings.TrimSpace(req.Args["command"])
		if command == "" {
			return Result{}, errors.New("run requires command")
		}
		timeoutStr := req.Args["timeout"]
		timeout := 60 * time.Second
		if timeoutStr != "" {
			if parsed, parseErr := time.ParseDuration(timeoutStr); parseErr == nil {
				timeout = parsed
			}
		}
		cmd := exec.Command("sh", "-c", command)
		cmd.Dir = mountRoot(req.Mounts)
		cmd.Stdin = nil
		var stdoutBuf, stderrBuf bytes.Buffer
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		err := cmd.Run()
		exitCode := 0
		if err != nil {
			if ctx.Err() != nil {
				return Result{}, errors.New("command execution timed out")
			}
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
		}
		return Result{
			Tool:    req.Tool,
			Summary: fmt.Sprintf("ran command (exit: %d)", exitCode),
			Output: map[string]string{
				"command": command,
				"stdout":  stdoutBuf.String(),
				"stderr":  stderrBuf.String(),
				"exit_code": fmt.Sprintf("%d", exitCode),
			},
		}, nil
	default:
		return Result{}, errors.New("unknown tool")
	}
}

func (r *Runtime) ExecuteAndRecord(store *artifact.Store, producerPID string, req Request) (Result, artifact.Artifact, error) {
	result, err := r.Execute(req)
	if err != nil {
		return Result{}, artifact.Artifact{}, err
	}
	record, err := store.CreateJSON("tool_result", producerPID, result, nil, map[string]string{
		"tool": result.Tool,
	})
	if err != nil {
		return Result{}, artifact.Artifact{}, err
	}
	return result, record, nil
}

func mountRoot(mounts []capability.Mount) string {
	if len(mounts) == 0 {
		return "."
	}
	return filepath.Clean(mounts[0].HostPath)
}
