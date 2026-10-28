package render

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestFitFooterFillsWindowLessLastColumn(t *testing.T) {
	if got := fitFooter(" status ", 20); got != " status"+strings.Repeat(" ", 12) {
		t.Fatalf("short rows should pad to width-1, got %q", got)
	}
	if got := fitFooter(strings.Repeat("x", 40), 20); ansi.StringWidth(got) != 19 || !strings.HasSuffix(got, "...") {
		t.Fatalf("long rows should truncate to width-1 with an ellipsis, got %q", got)
	}
	// "▸" and "›" are single-width; CJK is double-width and must still fit.
	for _, row := range []string{" workflow ▸ coordinator 3/400 › writer ", " 状态 模型 "} {
		if got := ansi.StringWidth(fitFooter(row, 50)); got != 49 {
			t.Fatalf("fitFooter(%q) is %d columns wide, want 49", row, got)
		}
	}
	if got := fitFooter(" status ", 0); got != " status " {
		t.Fatalf("unknown width should leave the row unchanged, got %q", got)
	}
}

func TestFooterRowExtendsBackgroundToEdge(t *testing.T) {
	row := Palette{Enabled: true}.FooterRow("x")
	if !strings.HasSuffix(row, "x\x1b[K\x1b[0m") || !strings.HasPrefix(row, "\x1b[0;38;5;255;48;5;24m") {
		t.Fatalf("row should erase to the edge before resetting the background, got %q", row)
	}
	if got := (Palette{}).FooterRow("x"); got != "x" {
		t.Fatalf("plain palette should not add escapes, got %q", got)
	}
}
