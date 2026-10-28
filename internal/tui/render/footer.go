package render

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"aos/internal/agentflow"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tokenizer"
	"github.com/charmbracelet/x/ansi"
)

type FooterStatus struct {
	Process      kernel.Process
	Model        string
	CheckpointID string
	Session      runtime.Session
	Counter      tokenizer.Counter
	Backend      agentflow.PromptBackend
}

type FooterBar struct {
	ui       Palette
	mu       sync.Mutex
	status   FooterStatus
	progress *WorkflowProgress
	pinned   bool
	rows     int
}

func NewFooterBar(ui Palette) *FooterBar {
	return &FooterBar{ui: ui}
}

// SetProgress adds a workflow progress row above the status row.
func (f *FooterBar) SetProgress(progress *WorkflowProgress) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.progress = progress
}

func (f *FooterBar) height() int {
	if f.progress != nil {
		return 2
	}
	return 1
}

func (f *FooterBar) Pin() {
	if !f.ui.Enabled {
		return
	}
	_, rows := TerminalSize()
	if rows < f.height()+1 {
		return
	}
	fmt.Fprint(os.Stdout, ansi.SetTopBottomMargins(1, rows-f.height()), ansi.CursorPosition(1, rows-f.height()))
	f.pinned = true
	f.rows = rows
}

func (f *FooterBar) Unpin() {
	if !f.ui.Enabled || !f.pinned {
		return
	}
	_, rows := TerminalSize()
	var builder strings.Builder
	builder.WriteString(ansi.SetTopBottomMargins(0, 0))
	builder.WriteString(ansi.SaveCursor)
	for row := rows - f.height() + 1; row <= rows; row++ {
		builder.WriteString(ansi.CursorPosition(1, row) + ansi.EraseEntireLine)
	}
	builder.WriteString(ansi.RestoreCursor)
	fmt.Fprint(os.Stdout, builder.String())
	f.pinned = false
	f.rows = 0
}

func (f *FooterBar) Update(status FooterStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
	f.drawLocked()
}

func (f *FooterBar) Draw() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drawLocked()
}

func (f *FooterBar) Println(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ui.Enabled {
		fmt.Println(text)
		fmt.Println(f.ui.Footer(truncateFooter(formatFooter(f.status))))
		return
	}
	fmt.Fprint(os.Stdout, ansi.EraseEntireLine, "\r")
	fmt.Println(text)
	f.drawLocked()
}

func (f *FooterBar) drawLocked() {
	if !f.ui.Enabled {
		return
	}
	if !f.pinned {
		f.Pin()
	}
	width, rows := TerminalSize()
	if f.pinned && rows != f.rows && rows >= f.height()+1 {
		fmt.Fprint(os.Stdout, ansi.SetTopBottomMargins(1, rows-f.height()))
		f.rows = rows
	}
	lines := make([]string, 0, 2)
	if f.progress != nil {
		lines = append(lines, f.ui.FooterRow(fitFooter(f.progress.StatusLine(), width)))
	}
	lines = append(lines, f.ui.FooterRow(fitFooter(formatFooter(f.status), width)))
	// One write per redraw so a concurrent redraw cannot split the
	// save/restore cursor sequence of another writer.
	var builder strings.Builder
	builder.WriteString(ansi.SaveCursor)
	for idx, line := range lines {
		builder.WriteString(ansi.CursorPosition(1, rows-len(lines)+1+idx) + ansi.EraseEntireLine + line)
	}
	builder.WriteString(ansi.RestoreCursor)
	fmt.Fprint(os.Stdout, builder.String())
}

func formatFooter(status FooterStatus) string {
	messageCount := len(status.Session.Messages)
	promptMessages := status.Backend.PromptMessages(runtime.GenerateRequest{
		Process:    status.Process,
		History:    append([]runtime.Message(nil), status.Session.Messages...),
		MemoryRefs: append([]runtime.MemoryRef(nil), status.Session.MemoryRefs...),
	})
	contextChars := estimateMessageChars(promptMessages)
	contextTokens := estimateTokens(contextChars)
	exact := false
	if status.Counter != nil {
		count, err := status.Counter.CountMessages(promptMessages)
		if err == nil {
			contextTokens = count.Tokens
			exact = count.Exact
		}
	}
	checkpoint := "none"
	if status.CheckpointID != "" {
		checkpoint = shortID(status.CheckpointID)
	}
	tokenLabel := "~"
	if exact {
		tokenLabel = "="
	}
	return fmt.Sprintf(
		" status | pid=%s | model=%s | ctx%s%d tok (%d chars) | msgs=%d | checkpoint=%s ",
		status.Process.PID,
		status.Model,
		tokenLabel,
		contextTokens,
		contextChars,
		messageCount,
		checkpoint,
	)
}

func estimateMessageChars(messages []runtime.Message) int {
	total := 0
	for _, message := range messages {
		total += len(message.Role)
		total += len(message.Content)
	}
	return total
}

func estimateTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return (chars + 3) / 4
}

func shortID(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}

// fitFooter pads or truncates a row to one column short of the window width,
// so the bar spans the window without writing the bottom-right cell, which
// makes some terminals scroll.
func fitFooter(value string, width int) string {
	limit := width - 1
	if limit <= 0 {
		return value
	}
	visible := ansi.StringWidth(value)
	if visible > limit {
		if limit <= 3 {
			return ansi.Truncate(value, limit, "")
		}
		return ansi.Truncate(value, limit, "...")
	}
	return value + strings.Repeat(" ", limit-visible)
}

func truncateFooter(value string) string {
	width, _ := TerminalSize()
	if width <= 0 || ansi.StringWidth(value) <= width {
		return value
	}
	if width <= 3 {
		return ansi.Truncate(value, width, "")
	}
	return ansi.Truncate(value, width, "...")
}
