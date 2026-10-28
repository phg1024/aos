package render

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"aos/internal/inference"
	"aos/internal/runtime"
	"aos/internal/tools"
	"github.com/charmbracelet/x/ansi"
)

// spinnerFrames animate the thinking line; spinnerInterval is how often it
// advances while the model thinks.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const spinnerInterval = 120 * time.Millisecond

type StreamRenderer struct {
	thinking      string
	thinkingLen   int
	thinkingStart time.Time
	frame         int
	stopSpinner   chan struct{}
	pending       string
	inThink       bool
	active        bool
	mu            sync.Mutex
	ui            Palette
	footer        *FooterBar
}

func NewStreamRenderer(ui Palette) *StreamRenderer {
	return &StreamRenderer{ui: ui}
}

func (r *StreamRenderer) SetFooter(footer *FooterBar) {
	r.footer = footer
}

func (r *StreamRenderer) Handle(event inference.StreamEvent) {
	switch event.Kind {
	case inference.StreamThinking:
		r.appendThinking(event.Text)
	case inference.StreamContent:
		r.consumeContent(event.Text)
	case inference.StreamDone:
		r.Clear()
	}
}

func (r *StreamRenderer) PrintStatus(line string) {
	r.Clear()
	if r.footer != nil {
		r.footer.Println(line)
		return
	}
	fmt.Println(line)
}

func (r *StreamRenderer) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active && r.ui.Enabled {
		fmt.Fprint(os.Stdout, ansi.EraseEntireLine, "\r")
	}
	if r.stopSpinner != nil {
		close(r.stopSpinner)
		r.stopSpinner = nil
	}
	r.active = false
	r.thinking = ""
	r.thinkingLen = 0
	r.pending = ""
	r.inThink = false
}

func (r *StreamRenderer) consumeContent(text string) {
	r.pending += text
	for {
		if !r.inThink {
			thinkPos := strings.Index(r.pending, "<think>")
			jsonPos := strings.Index(r.pending, "{")
			switch {
			case thinkPos == 0:
				r.pending = r.pending[len("<think>"):]
				r.inThink = true
				continue
			case thinkPos > 0 && (jsonPos == -1 || thinkPos < jsonPos):
				r.appendThinking(r.pending[:thinkPos])
				r.pending = r.pending[thinkPos+len("<think>"):]
				r.inThink = true
				continue
			case jsonPos > 0 && (thinkPos == -1 || jsonPos < thinkPos):
				r.appendThinking(r.pending[:jsonPos])
				r.pending = r.pending[jsonPos:]
				return
			case jsonPos == 0:
				return
			case thinkPos == -1 && jsonPos == -1:
				safe, tail := splitForTagBoundary(r.pending, maxInt(len("<think>")-1, 1))
				r.appendThinking(safe)
				r.pending = tail
				return
			default:
				return
			}
		}

		end := strings.Index(r.pending, "</think>")
		if end == -1 {
			safe, tail := splitForTagBoundary(r.pending, len("</think>")-1)
			r.appendThinking(safe)
			r.pending = tail
			return
		}

		r.appendThinking(r.pending[:end])
		r.pending = r.pending[end+len("</think>"):]
		r.inThink = false
	}
}

func (r *StreamRenderer) appendThinking(text string) {
	if !r.ui.Enabled {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if strings.TrimSpace(text) == "" {
		return
	}
	if !r.active {
		r.thinkingStart = time.Now()
		r.frame = 0
		r.startSpinnerLocked()
	}
	r.thinkingLen += len(text)
	r.renderThinkingLocked()
}

// startSpinnerLocked keeps the thinking line moving between stream events
// until Clear stops it.
func (r *StreamRenderer) startSpinnerLocked() {
	stop := make(chan struct{})
	r.stopSpinner = stop
	go func() {
		ticker := time.NewTicker(spinnerInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				r.mu.Lock()
				if r.active {
					r.frame++
					r.renderThinkingLocked()
				}
				r.mu.Unlock()
			}
		}
	}()
}

func (r *StreamRenderer) renderThinkingLocked() {
	if !r.ui.Enabled {
		return
	}
	r.active = true
	width, _ := TerminalSize()
	line := thinkingLine(r.frame, time.Since(r.thinkingStart), r.thinkingLen)
	line = clampDisplayWidth(line, maxInt(width-1, 32))
	fmt.Fprint(os.Stdout, ansi.EraseEntireLine+"\r"+r.ui.ThinkingPhase(line))
	if r.footer != nil {
		r.footer.Draw()
	}
}

func splitForTagBoundary(input string, tailLen int) (string, string) {
	if len(input) <= tailLen {
		return "", input
	}
	return input[:len(input)-tailLen], input[len(input)-tailLen:]
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

// thinkingLine reads like "⠹ thinking 14s · ~580 tokens"; tokens are
// estimated at four characters each.
func thinkingLine(frame int, elapsed time.Duration, chars int) string {
	spinner := spinnerFrames[frame%len(spinnerFrames)]
	return fmt.Sprintf("%s thinking %s · ~%s tokens", spinner, formatElapsedShort(elapsed), formatCount((chars+3)/4))
}

func formatElapsedShort(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func formatCount(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

func clampDisplayWidth(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}

func SummarizeToolCall(call runtime.ToolCall) string {
	if len(call.Args) == 0 {
		return call.Name
	}
	keys := make([]string, 0, len(call.Args))
	for key := range call.Args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(call.Args))
	for _, key := range keys {
		value := strings.ReplaceAll(call.Args[key], "\n", " ")
		if len(value) > 40 {
			value = value[:40] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%q", key, value))
	}
	return call.Name + " " + strings.Join(parts, " ")
}

func SummarizeToolResult(result tools.Result) string {
	switch result.Tool {
	case "find_files":
		matches := strings.TrimSpace(result.Output["matches"])
		if matches == "" {
			return "find_files found no matches"
		}
		return fmt.Sprintf("find_files found %d matches", len(strings.Split(matches, "\n")))
	case "list_dir":
		entries := strings.TrimSpace(result.Output["entries"])
		if entries == "" {
			return "list_dir found nothing"
		}
		return fmt.Sprintf("list_dir listed %d entries", len(strings.Split(entries, "\n")))
	case "read_file":
		return fmt.Sprintf("read_file loaded %s (%d chars)", filepath.Base(result.Output["path"]), len(result.Output["content"]))
	case "write_file":
		if bytes := strings.TrimSpace(result.Output["bytes"]); bytes != "" {
			return fmt.Sprintf("write_file wrote %s (%s chars)", filepath.Base(result.Output["path"]), bytes)
		}
		return fmt.Sprintf("write_file wrote %s", filepath.Base(result.Output["path"]))
	case "edit_file":
		if count := strings.TrimSpace(result.Output["occurrences"]); count != "" {
			return fmt.Sprintf("edit_file updated %s (%s matches)", filepath.Base(result.Output["path"]), count)
		}
		return fmt.Sprintf("edit_file updated %s", filepath.Base(result.Output["path"]))
	case "web_search":
		if strings.TrimSpace(result.Output["result"]) == "" {
			return "web_search returned no direct answer"
		}
		return "web_search returned results via " + result.Output["source"] + previewText(result.Output["result"], 72)
	case "web_fetch":
		summary := "web_fetch returned status " + result.Output["status"]
		if contentType := strings.TrimSpace(result.Output["content_type"]); contentType != "" {
			summary += " " + contentType
		}
		if text := strings.TrimSpace(result.Output["text"]); text != "" {
			summary += previewText(text, 72)
		}
		return summary
	case "run":
		summary := result.Tool + " exit=" + result.Output["exit_code"]
		if stdout := firstNonEmptyLine(result.Output["stdout"]); stdout != "" {
			summary += previewText(stdout, 72)
		} else if stderr := firstNonEmptyLine(result.Output["stderr"]); stderr != "" {
			summary += previewText(stderr, 72)
		}
		return summary
	case "spawn_agent":
		return fmt.Sprintf("spawn_agent launched %s (%s)", result.Output["agent_id"], result.Output["pid"])
	case "message_agent":
		return fmt.Sprintf("message_agent sent to %s", result.Output["to"])
	case "await_agent":
		return fmt.Sprintf("await_agent saw %s state=%s", result.Output["pid"], result.Output["state"])
	case "checkpoint":
		return "checkpoint saved " + result.Output["checkpoint_id"]
	case "list_artifacts":
		return "list_artifacts found " + result.Output["count"]
	case "publish_artifact":
		return fmt.Sprintf("publish_artifact created %s (%s)", result.Output["artifact_id"], result.Output["type"])
	case "acquire_lease":
		if result.Output["held"] == "true" {
			return fmt.Sprintf("acquire_lease blocked by %s", result.Output["owner_pid"])
		}
		return fmt.Sprintf("acquire_lease locked %s", result.Output["resource"])
	case "release_lease":
		return fmt.Sprintf("release_lease unlocked %s", result.Output["resource"])
	default:
		return result.Summary
	}
}

func firstNonEmptyLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func previewText(value string, limit int) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) > limit {
		value = value[:limit] + "..."
	}
	return " [" + value + "]"
}
