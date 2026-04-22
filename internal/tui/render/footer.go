package render

import (
	"fmt"
	"os"
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
	ui     Palette
	mu     sync.Mutex
	status FooterStatus
	pinned bool
	rows   int
}

func NewFooterBar(ui Palette) *FooterBar {
	return &FooterBar{ui: ui}
}

func (f *FooterBar) Pin() {
	if !f.ui.Enabled {
		return
	}
	_, rows := TerminalSize()
	if rows < 2 {
		return
	}
	fmt.Fprint(os.Stdout, ansi.SetTopBottomMargins(1, rows-1), ansi.CursorPosition(1, rows-1))
	f.pinned = true
	f.rows = rows
}

func (f *FooterBar) Unpin() {
	if !f.ui.Enabled || !f.pinned {
		return
	}
	_, rows := TerminalSize()
	fmt.Fprint(os.Stdout, ansi.SetTopBottomMargins(0, 0))
	fmt.Fprint(os.Stdout, ansi.SaveCursor, ansi.CursorPosition(1, rows), ansi.EraseEntireLine, ansi.RestoreCursor)
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
	line := f.ui.Footer(truncateFooter(formatFooter(f.status)))
	if !f.ui.Enabled {
		return
	}
	if !f.pinned {
		f.Pin()
	}
	_, rows := TerminalSize()
	if f.pinned && rows != f.rows && rows >= 2 {
		fmt.Fprint(os.Stdout, ansi.SetTopBottomMargins(1, rows-1))
		f.rows = rows
	}
	fmt.Fprint(os.Stdout, ansi.SaveCursor, ansi.CursorPosition(1, rows), ansi.EraseEntireLine)
	fmt.Fprint(os.Stdout, line)
	fmt.Fprint(os.Stdout, ansi.RestoreCursor)
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

func truncateFooter(value string) string {
	width, _ := TerminalSize()
	if width <= 0 || len(value) <= width {
		return value
	}
	if width <= 3 {
		return value[:width]
	}
	return value[:width-3] + "..."
}
