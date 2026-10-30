package render

import "fmt"

// picker is a list the user moves through with the arrow keys and picks
// from with Enter; Esc or q cancels. It keeps the selected row in view.
type picker struct {
	items    []string
	selected int
	top      int
}

// handle applies one key. done reports that the pick ended; chosen is the
// picked index, or -1 when cancelled.
func (p *picker) handle(k key) (done bool, chosen int) {
	switch {
	case k.kind == keyEnter:
		return true, p.selected
	case k.kind == keyEscape, k.kind == keyRune && k.r == 'q':
		return true, -1
	case k.kind == keyUp, k.kind == keyRune && k.r == 'k':
		p.selected = max(p.selected-1, 0)
	case k.kind == keyDown, k.kind == keyRune && k.r == 'j':
		p.selected = min(p.selected+1, len(p.items)-1)
	case k.kind == keyHome:
		p.selected = 0
	case k.kind == keyEnd:
		p.selected = len(p.items) - 1
	case k.kind == keyRune && k.r >= '1' && k.r <= '9':
		if n := int(k.r - '1'); n < len(p.items) {
			p.selected = n
		}
	}
	return false, 0
}

// rows lays the picker out in at most height terminal rows: a title, the
// items in view with the selected one marked, and how many are out of view.
func (p *picker) rows(ui Palette, title string, height int) []string {
	visible := max(height-4, 3)
	if p.selected < p.top {
		p.top = p.selected
	}
	if p.selected >= p.top+visible {
		p.top = p.selected - visible + 1
	}
	p.top = max(min(p.top, len(p.items)-visible), 0)
	end := min(p.top+visible, len(p.items))

	rows := []string{ui.Header(" "+title+" ") + ui.Subtle("  ↑/↓ choose · Enter resume · Esc cancel")}
	if p.top > 0 {
		rows = append(rows, ui.Subtle(fmt.Sprintf("   ↑ %d more", p.top)))
	}
	for i := p.top; i < end; i++ {
		if i == p.selected {
			rows = append(rows, ui.CommandName("❯ ")+p.items[i])
		} else {
			rows = append(rows, "  "+p.items[i])
		}
	}
	if below := len(p.items) - end; below > 0 {
		rows = append(rows, ui.Subtle(fmt.Sprintf("   ↓ %d more", below)))
	}
	return rows
}

// clearFrame replaces drawn picker rows with one empty row, leaving the
// cursor at its start, so what is printed next takes the picker's place.
func clearFrame(drawnRows int) string {
	return inputFrame(drawnRows, 0, []string{""}, 0, 0)
}
