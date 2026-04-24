package render

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type Palette struct {
	Enabled bool
}

func NewPalette(file *os.File) Palette {
	info, err := file.Stat()
	if err != nil {
		return Palette{}
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return Palette{}
	}
	return Palette{Enabled: info.Mode()&os.ModeCharDevice != 0}
}

func (p Palette) Header(text string) string {
	return p.wrap(text, "1;38;5;255;48;5;24")
}

func (p Palette) Footer(text string) string {
	return p.wrap(text, "0;38;5;255;48;5;24")
}

func (p Palette) Banner(text string) string {
	if !p.Enabled {
		return text
	}
	lines := strings.Split(text, "\n")
	styles := []string{
		"1;38;5;39",
		"1;38;5;45",
		"1;38;5;81",
		"1;38;5;75",
		"1;38;5;111",
		"1;38;5;67",
		"1;38;5;39",
	}
	for idx, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines[idx] = "\033[" + styles[idx%len(styles)] + "m" + line + "\033[0m"
	}
	return strings.Join(lines, "\n")
}

func (p Palette) Prompt(text string) string {
	return p.wrap(text, "1;38;5;39")
}

func (p Palette) Agent(text string) string {
	return p.wrap(text, "1;38;5;255")
}

func (p Palette) AgentBlock(prefix, body string) string {
	if !p.Enabled {
		return prefix + body
	}
	return p.Agent(prefix) + body
}

func (p Palette) ToolCall(text string) string {
	return p.wrap(text, "1;38;5;45")
}

func (p Palette) ToolResult(text string) string {
	return p.wrap(text, "1;38;5;79")
}

func (p Palette) ToolError(text string) string {
	return p.wrap(text, "1;38;5;203")
}

func (p Palette) ThinkingPhase(text string, _ int) string {
	if !p.Enabled {
		return text
	}
	return lipgloss.NewStyle().
		Blink(true).
		Faint(true).
		Foreground(lipgloss.Color("67")).
		Render(text)
}

func (p Palette) Warn(text string) string {
	return p.wrap(text, "1;38;5;221")
}

func (p Palette) Error(text string) string {
	return p.wrap(text, "1;38;5;255;48;5;160")
}

func (p Palette) Subtle(text string) string {
	return p.wrap(text, "2;38;5;110")
}

func (p Palette) wrap(text, code string) string {
	if !p.Enabled {
		return text
	}
	return "\033[" + code + "m" + text + "\033[0m"
}

func AOSBanner() string {
	return strings.Trim(`
     ██████╗  ██████╗ ███████╗
     ██╔══██╗██╔═══██╗██╔════╝
     ███████║██║   ██║███████╗
     ██╔══██║██║   ██║╚════██║
     ██║  ██║╚██████╔╝███████║
     ╚═╝  ╚═╝ ╚═════╝ ╚══════╝

     agent operating system
 `, "\n")
}
