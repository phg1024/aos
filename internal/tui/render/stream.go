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

type StreamRenderer struct {
	thinking    string
	thinkingRaw string
	pending     string
	inThink     bool
	active      bool
	pulseFrame  int
	pulseStop   chan struct{}
	mu          sync.Mutex
	ui          Palette
	footer      *FooterBar
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
	if r.pulseStop != nil {
		close(r.pulseStop)
		r.pulseStop = nil
	}
	if r.active && r.ui.Enabled {
		fmt.Fprint(os.Stdout, ansi.EraseEntireLine, "\r")
	}
	r.active = false
	r.pulseFrame = 0
	r.thinking = ""
	r.thinkingRaw = ""
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
	r.thinkingRaw += text
	if len(r.thinkingRaw) > 2000 {
		r.thinkingRaw = r.thinkingRaw[len(r.thinkingRaw)-1200:]
	}
	preview := previewThought(r.thinkingRaw, 3, 256)
	width, _ := TerminalSize()
	preview = clampDisplayWidth(preview, maxInt(width-len("thinking> ")-1, 32))
	if preview == r.thinking && r.active {
		return
	}
	r.thinking = preview
	r.renderThinkingLocked()
	r.ensurePulseLocked()
}

func (r *StreamRenderer) ensurePulseLocked() {
	if r.pulseStop != nil || !r.ui.Enabled || r.thinking == "" {
		return
	}
	r.active = true
	r.pulseStop = make(chan struct{})
	stop := r.pulseStop
	go r.pulseLoop(stop)
}

func (r *StreamRenderer) pulseLoop(stop chan struct{}) {
	ticker := time.NewTicker(180 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.mu.Lock()
			if !r.active || r.thinking == "" || r.pulseStop != stop {
				r.mu.Unlock()
				return
			}
			r.pulseFrame = (r.pulseFrame + 1) % 4
			r.renderThinkingLocked()
			r.mu.Unlock()
		case <-stop:
			return
		}
	}
}

func (r *StreamRenderer) renderThinkingLocked() {
	if r.thinking == "" || !r.ui.Enabled {
		return
	}
	r.active = true
	fmt.Fprint(os.Stdout, ansi.EraseEntireLine, "\r")
	fmt.Fprint(os.Stdout, r.ui.ThinkingPhase("thinking> "+r.thinking, r.pulseFrame))
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

func previewThought(raw string, maxThoughts, maxThoughtLen int) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	paragraphs := strings.Split(raw, "\n\n")
	var preview []string
	for idx := len(paragraphs) - 1; idx >= 0; idx-- {
		candidate := strings.TrimSpace(paragraphs[idx])
		if candidate == "" {
			continue
		}
		candidate = strings.Join(strings.Fields(candidate), " ")
		if len(candidate) > maxThoughtLen {
			candidate = candidate[:maxThoughtLen] + "..."
		}
		preview = append(preview, candidate)
		if len(preview) >= maxThoughts {
			break
		}
	}
	if len(preview) == 0 {
		return ""
	}
	for left, right := 0, len(preview)-1; left < right; left, right = left+1, right-1 {
		preview[left], preview[right] = preview[right], preview[left]
	}
	return strings.Join(preview, "")
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
