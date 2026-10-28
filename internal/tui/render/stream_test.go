package render

import (
	"io"
	"os"
	"testing"
	"time"

	"aos/internal/inference"
)

func TestThinkingLine(t *testing.T) {
	if got := thinkingLine(2, 14*time.Second+400*time.Millisecond, 2320); got != "⠹ thinking 14s · ~580 tokens" {
		t.Fatalf("unexpected line %q", got)
	}
	if got := thinkingLine(len(spinnerFrames), 75*time.Second, 9200); got != "⠋ thinking 1m15s · ~2.3k tokens" {
		t.Fatalf("spinner should wrap and long runs use minutes, got %q", got)
	}
}

func TestThinkingSpinnerAdvancesAndStopsOnClear(t *testing.T) {
	stdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = writer
	go io.Copy(io.Discard, reader)
	defer func() { os.Stdout = stdout; writer.Close() }()

	renderer := NewStreamRenderer(Palette{Enabled: true})
	renderer.Handle(inference.StreamEvent{Kind: inference.StreamThinking, Text: "planning the fix"})
	time.Sleep(4 * spinnerInterval)
	renderer.mu.Lock()
	advanced := renderer.frame
	renderer.mu.Unlock()
	if advanced == 0 {
		t.Fatal("spinner did not advance without new stream events")
	}

	renderer.Clear()
	time.Sleep(3 * spinnerInterval)
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	if renderer.active || renderer.stopSpinner != nil {
		t.Fatal("Clear should stop the spinner")
	}
}
