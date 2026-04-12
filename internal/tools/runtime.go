package tools

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
		bytes, err := os.ReadFile(path)
		if err != nil {
			return Result{}, err
		}
		return Result{
			Tool:    req.Tool,
			Summary: "read file",
			Output: map[string]string{
				"path":    path,
				"content": string(bytes),
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
