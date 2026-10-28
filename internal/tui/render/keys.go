package render

const keyEsc = 0x1b

// isLoneEsc reports whether one read from the terminal is a bare Esc press.
// Arrow and function keys also start with Esc but arrive as longer
// sequences in the same read, so they do not count.
func isLoneEsc(input []byte) bool {
	return len(input) == 1 && input[0] == keyEsc
}
