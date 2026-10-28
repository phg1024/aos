package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/sandbox"
)

// runWaitDelay bounds how long the run tool waits for stdout/stderr to close
// after the shell exits or is killed.
var runWaitDelay = 2 * time.Second

// MaxToolOutputBytes caps file contents and command output returned to an
// agent, so a single large result cannot overflow the model context.
const MaxToolOutputBytes = 64 * 1024

// Search endpoints are variables so tests can point them at a local server.
var (
	instantAnswerEndpoint = "https://api.duckduckgo.com/"
	htmlSearchEndpoint    = "https://html.duckduckgo.com/html/"
)

var ErrToolDenied = errors.New("tool execution denied by capabilities")

var ErrPathDenied = errors.New("path access denied by capabilities")

type Request struct {
	Tool          string
	Args          map[string]string
	CapabilitySet capability.Set
	Mounts        []capability.Mount
	// PID is the calling process; run keeps a scratch directory per PID.
	PID string
}

type Result struct {
	Tool    string            `json:"tool"`
	Summary string            `json:"summary"`
	Output  map[string]string `json:"output,omitempty"`
}

type Runtime struct {
	evaluator capability.Evaluator
	// Protected paths (the state directory) are out of reach of every
	// tool, even where a mount and scope would grant them.
	Protected []string
	// ModelHosts are the model endpoint's hosts, which web_fetch refuses
	// unless the capability set's NetworkAllow names them.
	ModelHosts []string
	// Confine builds the confined command for run; nil means
	// sandbox.Command. Tests replace it to simulate a platform without a
	// sandbox.
	Confine func(ctx context.Context, policy sandbox.Policy, dir, command string) (*exec.Cmd, error)
	// ScratchRoot holds each process's run scratch directory; empty means
	// a temporary directory made on first use.
	ScratchRoot string
	scratchMu   sync.Mutex
}

// ErrRunUnconfined means run was refused: it cannot be confined here and
// the process may not run unconfined.
var ErrRunUnconfined = errors.New("run refused: it cannot be confined here and the process lacks unconfined_run")

// ErrNetworkDenied means a web tool was refused network access.
var ErrNetworkDenied = errors.New("network access denied by capabilities")

// MaxFetchBytes caps how much of a web_fetch response body is read.
const MaxFetchBytes = 4 << 20

func (r *Runtime) canRead(path string, req Request) bool {
	return r.evaluator.CanRead(path, req.Mounts, req.CapabilitySet) && !r.protected(path)
}

func (r *Runtime) canWrite(path string, req Request) bool {
	return r.evaluator.CanWrite(path, req.Mounts, req.CapabilitySet) && !r.protected(path)
}

// protected reports whether path is, or lies inside, a protected path.
func (r *Runtime) protected(path string) bool {
	resolved := capability.ResolvePath(path)
	for _, root := range r.Protected {
		if capability.Within(capability.ResolvePath(root), resolved) {
			return true
		}
	}
	return false
}

func NewRuntime() *Runtime {
	return &Runtime{evaluator: capability.Evaluator{}}
}

// ErrToolCancelled reports that the caller's context stopped a tool.
var ErrToolCancelled = errors.New("tool cancelled")

func (r *Runtime) Execute(req Request) (Result, error) {
	return r.ExecuteContext(context.Background(), req)
}

// ExecuteContext runs a tool; cancelling ctx kills a running command.
func (r *Runtime) ExecuteContext(ctx context.Context, req Request) (Result, error) {
	if !r.evaluator.ToolAllowed(req.Tool, req.CapabilitySet) {
		return Result{}, ErrToolDenied
	}

	switch req.Tool {
	case "write_file":
		path := filepath.Clean(req.Args["path"])
		content, ok := req.Args["content"]
		if !ok {
			return Result{}, errors.New("write_file requires content")
		}
		if !r.canWrite(path, req) {
			return Result{}, ErrPathDenied
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return Result{}, err
		}
		return Result{
			Tool:    req.Tool,
			Summary: "wrote file",
			Output: map[string]string{
				"path":  path,
				"bytes": fmt.Sprintf("%d", len(content)),
			},
		}, nil
	case "read_file":
		path := filepath.Clean(req.Args["path"])
		if !r.canRead(path, req) {
			return Result{}, ErrPathDenied
		}
		contentBytes, err := os.ReadFile(path)
		if err != nil {
			return Result{}, err
		}
		content, truncated := capToolOutput(string(contentBytes), "use run with head, tail, sed -n, or a script to inspect the rest")
		output := map[string]string{
			"path":    path,
			"content": content,
		}
		if truncated {
			output["truncated"] = "true"
			output["size_bytes"] = fmt.Sprintf("%d", len(contentBytes))
		}
		return Result{
			Tool:    req.Tool,
			Summary: "read file",
			Output:  output,
		}, nil
	case "edit_file":
		path := filepath.Clean(req.Args["path"])
		oldText := req.Args["old_text"]
		newText := req.Args["new_text"]
		replaceAll := strings.EqualFold(strings.TrimSpace(req.Args["replace_all"]), "true")
		if path == "" {
			return Result{}, errors.New("edit_file requires path")
		}
		if oldText == "" {
			return Result{}, errors.New("edit_file requires old_text")
		}
		if !r.canRead(path, req) || !r.canWrite(path, req) {
			return Result{}, ErrPathDenied
		}
		contentBytes, err := os.ReadFile(path)
		if err != nil {
			return Result{}, err
		}
		content := string(contentBytes)
		count := strings.Count(content, oldText)
		if count == 0 {
			return Result{}, errors.New("edit_file old_text not found")
		}
		if count > 1 && !replaceAll {
			return Result{}, errors.New("edit_file old_text matched multiple times; pass replace_all=true")
		}
		updated := content
		if replaceAll {
			updated = strings.ReplaceAll(content, oldText, newText)
		} else {
			updated = strings.Replace(content, oldText, newText, 1)
		}
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			return Result{}, err
		}
		return Result{
			Tool:    req.Tool,
			Summary: "edited file",
			Output: map[string]string{
				"path":        path,
				"occurrences": fmt.Sprintf("%d", count),
				"bytes":       fmt.Sprintf("%d", len(updated)),
			},
		}, nil
	case "list_dir":
		path := filepath.Clean(req.Args["path"])
		if path == "." || path == "" {
			path = mountRoot(req.Mounts)
		}
		if !r.canRead(path, req) {
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
		if !r.canRead(root, req) {
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
			if !r.canRead(path, req) {
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
	case "run":
		command := strings.TrimSpace(req.Args["command"])
		if command == "" {
			return Result{}, errors.New("run requires command")
		}
		timeout := 60 * time.Second
		if parsed, parseErr := time.ParseDuration(req.Args["timeout"]); parseErr == nil && parsed > 0 {
			timeout = parsed
		}
		bound := req.CapabilitySet.RunTimeoutBound()
		clamped := timeout > bound
		if clamped {
			timeout = bound
		}
		cmdCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd, confinement, err := r.runCommand(cmdCtx, req, command)
		if err != nil {
			return Result{}, err
		}
		isolateProcessGroup(cmd)
		// Bound the wait for output pipes in case a detached descendant still holds them.
		cmd.WaitDelay = runWaitDelay
		cmd.Stdin = nil
		var stdoutBuf, stderrBuf bytes.Buffer
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf
		err = cmd.Run()
		exitCode := 0
		if err != nil {
			if ctx.Err() != nil {
				return Result{}, fmt.Errorf("%w: %w", ErrToolCancelled, ctx.Err())
			}
			if cmdCtx.Err() != nil {
				return Result{}, fmt.Errorf("command execution timed out after %s", timeout)
			}
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
		}
		output := map[string]string{
			"command":       command,
			"stdout":        capOutput(stdoutBuf.String()),
			"stderr":        capOutput(stderrBuf.String()),
			"exit_code":     fmt.Sprintf("%d", exitCode),
			"confinement":   confinement,
			"timeout":       timeout.String(),
			"timeout_bound": bound.String(),
		}
		if clamped {
			output["timeout_clamped"] = "true"
		}
		return Result{
			Tool:    req.Tool,
			Summary: fmt.Sprintf("ran command (exit: %d)", exitCode),
			Output:  output,
		}, nil
	case "web_search":
		query := strings.TrimSpace(req.Args["query"])
		if query == "" {
			return Result{}, errors.New("web_search requires query")
		}
		if !req.CapabilitySet.AllowNetwork {
			return Result{}, fmt.Errorf("%w: web_search needs allow_network", ErrNetworkDenied)
		}
		timeoutStr := req.Args["timeout"]
		timeout := 30 * time.Second
		if timeoutStr != "" {
			if parsed, parseErr := time.ParseDuration(timeoutStr); parseErr == nil {
				timeout = parsed
			}
		}
		searchURL := instantAnswerEndpoint + "?q=" + url.QueryEscape(query) + "&format=json&no_html=1"
		client := &http.Client{
			Timeout: timeout,
		}
		resp, err := getWithUserAgent(client, searchURL)
		if err != nil {
			return Result{}, fmt.Errorf("web search failed: %w", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return Result{}, fmt.Errorf("failed to read search response: %w", err)
		}
		var result map[string]interface{}
		if err := json.Unmarshal(body, &result); err != nil {
			return Result{}, fmt.Errorf("failed to parse search response: %w", err)
		}
		summary := summarizeInstantAnswer(result)
		if strings.TrimSpace(summary) == "" {
			htmlSummary, htmlURL, _, htmlErr := fallbackHTMLSearch(client, query)
			if htmlErr == nil && strings.TrimSpace(htmlSummary) != "" {
				return Result{
					Tool:    req.Tool,
					Summary: "web search completed",
					Output: map[string]string{
						"query":      query,
						"status":     "200",
						"source":     "duckduckgo_html",
						"search_url": htmlURL,
						"result":     htmlSummary,
					},
				}, nil
			}
		}
		return Result{
			Tool:    req.Tool,
			Summary: "web search completed",
			Output: map[string]string{
				"query":      query,
				"status":     fmt.Sprintf("%d", resp.StatusCode),
				"source":     "duckduckgo_instant_answer",
				"search_url": searchURL,
				"result":     summary,
			},
		}, nil
	case "web_fetch":
		targetURL := strings.TrimSpace(req.Args["url"])
		if targetURL == "" {
			return Result{}, errors.New("web_fetch requires url")
		}
		parsedURL, parseErr := url.Parse(targetURL)
		if parseErr != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Hostname() == "" {
			return Result{}, errors.New("web_fetch requires a valid http or https url")
		}
		if !req.CapabilitySet.AllowNetwork {
			return Result{}, fmt.Errorf("%w: web_fetch needs allow_network", ErrNetworkDenied)
		}
		timeoutStr := req.Args["timeout"]
		timeout := 60 * time.Second
		if timeoutStr != "" {
			if parsed, parseErr := time.ParseDuration(timeoutStr); parseErr == nil {
				timeout = parsed
			}
		}
		client := r.fetchClient(req.CapabilitySet, timeout)
		resp, err := client.Get(targetURL)
		if err != nil {
			if errors.Is(err, ErrNetworkDenied) {
				return Result{}, fmt.Errorf("web fetch refused: %w", err)
			}
			return Result{}, fmt.Errorf("web fetch failed: %w", err)
		}
		defer resp.Body.Close()
		// Cap while reading, so a huge or endless body is never buffered.
		body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFetchBytes+1))
		if err != nil {
			return Result{}, fmt.Errorf("failed to read response: %w", err)
		}
		bodyTruncated := len(body) > MaxFetchBytes
		if bodyTruncated {
			body = body[:MaxFetchBytes]
		}
		contentType := resp.Header.Get("Content-Type")
		fullText := string(body)
		if strings.Contains(strings.ToLower(contentType), "html") || strings.Contains(strings.ToLower(fullText[:min(len(fullText), 512)]), "<html") {
			fullText = simpleHTMLToText(fullText)
		}
		offset := 0
		if raw := strings.TrimSpace(req.Args["offset"]); raw != "" {
			parsed, parseErr := strconv.Atoi(raw)
			if parseErr != nil || parsed < 0 {
				return Result{}, errors.New("web_fetch offset must be a non-negative byte offset")
			}
			offset = min(parsed, len(fullText))
			for offset > 0 && offset < len(fullText) && !utf8.RuneStart(fullText[offset]) {
				offset--
			}
		}
		next := min(offset+MaxToolOutputBytes, len(fullText))
		for next > offset && next < len(fullText) && !utf8.RuneStart(fullText[next]) {
			next--
		}
		text, truncated := capToolOutput(fullText[offset:], fmt.Sprintf("call web_fetch again with offset=%d to continue", next))
		output := map[string]string{
			"url":          targetURL,
			"status":       fmt.Sprintf("%d", resp.StatusCode),
			"content_type": contentType,
			"text":         text,
			"text_bytes":   fmt.Sprintf("%d", len(fullText)),
		}
		if truncated {
			output["truncated"] = "true"
		}
		if bodyTruncated {
			output["body_truncated"] = fmt.Sprintf("true: only the first %d bytes of the response were read", MaxFetchBytes)
		}
		return Result{
			Tool:    req.Tool,
			Summary: "web fetch completed",
			Output:  output,
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

// capToolOutput truncates s to MaxToolOutputBytes on a UTF-8 boundary and
// appends a notice with the dropped size and how to see more.
func capToolOutput(s, hint string) (string, bool) {
	if len(s) <= MaxToolOutputBytes {
		return s, false
	}
	cut := MaxToolOutputBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[truncated: showing %d of %d bytes; %s]", cut, len(s), hint), true
}

func capOutput(s string) string {
	capped, _ := capToolOutput(s, "redirect output to a file and inspect it in smaller pieces")
	return capped
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
	open := "<" + strings.ToLower(tag)
	close := "</" + strings.ToLower(tag)
	lower := strings.ToLower(s)
	for {
		startOpen := strings.Index(lower, open)
		if startOpen == -1 {
			break
		}
		endOpen := strings.Index(lower[startOpen:], ">")
		if endOpen == -1 {
			break
		}
		contentStart := startOpen + endOpen + 1
		startCloseRel := strings.Index(lower[contentStart:], close)
		if startCloseRel == -1 {
			s = s[:startOpen] + s[contentStart:]
			lower = strings.ToLower(s)
			continue
		}
		startClose := contentStart + startCloseRel
		endClose := strings.Index(lower[startClose:], ">")
		if endClose == -1 {
			s = s[:startOpen] + s[startClose+len(close):]
			lower = strings.ToLower(s)
			continue
		}
		s = s[:startOpen] + s[startClose+endClose+1:]
		lower = strings.ToLower(s)
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

func getWithUserAgent(client *http.Client, target string) (*http.Response, error) {
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 (compatible; AOS/1.0; +https://example.invalid/aos)")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	return client.Do(request)
}

func summarizeInstantAnswer(result map[string]interface{}) string {
	var summary strings.Builder
	if abstract, ok := result["Abstract"].(string); ok && abstract != "" {
		summary.WriteString("Abstract: " + abstract + "\n")
	}
	if abstractURL, ok := result["AbstractURL"].(string); ok && abstractURL != "" {
		summary.WriteString("URL: " + abstractURL + "\n")
	}
	if abstractSource, ok := result["AbstractSource"].(string); ok && abstractSource != "" {
		summary.WriteString("Source: " + abstractSource + "\n")
	}
	if related, ok := result["RelatedTopics"].([]interface{}); ok {
		count := 0
		for _, topic := range related {
			if count >= 5 {
				break
			}
			entry, ok := topic.(map[string]interface{})
			if !ok {
				continue
			}
			text, _ := entry["Text"].(string)
			firstURL, _ := entry["FirstURL"].(string)
			if text == "" && firstURL == "" {
				continue
			}
			if count == 0 {
				summary.WriteString("\nRelated topics:\n")
			}
			count++
			if text != "" {
				summary.WriteString("  - " + text + "\n")
			}
			if firstURL != "" {
				summary.WriteString("    URL: " + firstURL + "\n")
			}
		}
	}
	return strings.TrimSpace(summary.String())
}

func fallbackHTMLSearch(client *http.Client, query string) (summary string, searchURL string, raw string, err error) {
	searchURL = htmlSearchEndpoint + "?q=" + url.QueryEscape(query)
	resp, err := getWithUserAgent(client, searchURL)
	if err != nil {
		return "", searchURL, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", searchURL, "", err
	}
	raw = string(body)
	results := extractHTMLSearchResults(raw)
	if len(results) == 0 {
		return "", searchURL, raw, nil
	}
	var builder strings.Builder
	for i, result := range results {
		if i >= 5 {
			break
		}
		builder.WriteString(fmt.Sprintf("%d. %s\n", i+1, result.Title))
		if result.URL != "" {
			builder.WriteString("   URL: " + result.URL + "\n")
		}
		if result.Snippet != "" {
			builder.WriteString("   Snippet: " + result.Snippet + "\n")
		}
	}
	return strings.TrimSpace(builder.String()), searchURL, raw, nil
}

type htmlSearchResult struct {
	Title   string
	URL     string
	Snippet string
}

var (
	resultAnchorPattern  = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*(?:result__a|result-link)[^"]*"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	resultSnippetPattern = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*(?:result__snippet|result-snippet)[^"]*"[^>]*>(.*?)</a>|<div[^>]+class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</div>`)
)

func extractHTMLSearchResults(html string) []htmlSearchResult {
	anchors := resultAnchorPattern.FindAllStringSubmatch(html, 10)
	if len(anchors) == 0 {
		return nil
	}
	snippets := resultSnippetPattern.FindAllStringSubmatch(html, 10)
	results := make([]htmlSearchResult, 0, len(anchors))
	for i, match := range anchors {
		targetURL := decodeDuckDuckGoRedirect(htmlEntityDecode(match[1]))
		title := normalizeWhitespace(stripTags(htmlEntityDecode(match[2])))
		if title == "" || targetURL == "" {
			continue
		}
		snippet := ""
		if i < len(snippets) {
			snippet = htmlEntityDecode(snippets[i][1])
			if snippet == "" && len(snippets[i]) > 2 {
				snippet = htmlEntityDecode(snippets[i][2])
			}
			snippet = normalizeWhitespace(stripTags(snippet))
		}
		results = append(results, htmlSearchResult{
			Title:   title,
			URL:     targetURL,
			Snippet: snippet,
		})
	}
	return results
}

func decodeDuckDuckGoRedirect(target string) string {
	parsed, err := url.Parse(target)
	if err != nil {
		return target
	}
	if uddg := parsed.Query().Get("uddg"); uddg != "" {
		decoded, err := url.QueryUnescape(uddg)
		if err == nil && decoded != "" {
			return decoded
		}
		return uddg
	}
	return target
}

func htmlEntityDecode(input string) string {
	replacer := strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", "\"",
		"&#39;", "'",
		"&apos;", "'",
		"&nbsp;", " ",
	)
	return replacer.Replace(input)
}
