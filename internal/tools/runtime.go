package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	case "web_search":
		query := strings.TrimSpace(req.Args["query"])
		if query == "" {
			return Result{}, errors.New("web_search requires query")
		}
		timeoutStr := req.Args["timeout"]
		timeout := 30 * time.Second
		if timeoutStr != "" {
			if parsed, parseErr := time.ParseDuration(timeoutStr); parseErr == nil {
				timeout = parsed
			}
		}
		searchURL := "https://www.google.com/search"
		form := url.Values{}
		form.Set("q", query)
		form.Set("num", "10")
		client := &http.Client{
			Timeout: timeout,
		}
		reqURL := searchURL + "?" + form.Encode()
		resp, err := client.Get(reqURL)
		if err != nil {
			return Result{}, fmt.Errorf("web search failed: %w", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return Result{}, fmt.Errorf("failed to read search response: %w", err)
		}
		return Result{
			Tool:    req.Tool,
			Summary: "web search completed",
			Output: map[string]string{
				"query":    query,
				"status":   fmt.Sprintf("%d", resp.StatusCode),
				"raw_html": string(body),
			},
		}, nil
	case "web_fetch":
		targetURL := strings.TrimSpace(req.Args["url"])
		if targetURL == "" {
			return Result{}, errors.New("web_fetch requires url")
		}
		if _, parseErr := url.Parse(targetURL); parseErr != nil {
			return Result{}, errors.New("web_fetch requires valid url")
		}
		timeoutStr := req.Args["timeout"]
		timeout := 60 * time.Second
		if timeoutStr != "" {
			if parsed, parseErr := time.ParseDuration(timeoutStr); parseErr == nil {
				timeout = parsed
			}
		}
		client := &http.Client{
			Timeout: timeout,
		}
		resp, err := client.Get(targetURL)
		if err != nil {
			return Result{}, fmt.Errorf("web fetch failed: %w", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return Result{}, fmt.Errorf("failed to read response: %w", err)
		}
		contentType := resp.Header.Get("Content-Type")
		return Result{
			Tool:    req.Tool,
			Summary: "web fetch completed",
			Output: map[string]string{
				"url":         targetURL,
				"status":      fmt.Sprintf("%d", resp.StatusCode),
				"content_type": contentType,
				"content":     string(body),
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

// truncateString truncates a string to maxLen characters
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// simpleHTMLToText converts basic HTML to plain text
func simpleHTMLToText(html string) string {
	text := html
	// Remove script tags
	text = removeTag(text, "script")
	// Remove style tags
	text = removeTag(text, "style")
	// Remove all other tags
	text = stripTags(text)
	// Normalize whitespace
	text = normalizeWhitespace(text)
	return text
}

func removeTag(s, tag string) string {
	open := "<" + tag
	close := "</" + tag
	for {
		startOpen := strings.Index(s, open)
		if startOpen == -1 {
			break
		}
		endOpen := strings.Index(s[startOpen:], ">")
		if endOpen == -1 {
			break
		}
		startClose := strings.Index(s, close)
		if startClose == -1 {
			break
		}
		endClose := strings.Index(s[startClose:], ">")
		if endClose == -1 {
			break
		}
		s = s[:startOpen] + s[startOpen+len(open)+endOpen+1 : startClose] + s[startClose+len(close)+endClose+1:]
	}
	return s
}

func stripTags(s string) string {
	var result strings.Builder
	inTag := false
	for _, ch := range s {
		if ch == '<' {
			inTag = true
		} else if ch == '>' {
			inTag = false
		} else if !inTag {
			result.WriteRune(ch)
		}
	}
	return result.String()
}

func normalizeWhitespace(s string) string {
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}
