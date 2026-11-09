package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The command-line grammar that the instrument rules read a run's command
// with (agentflow's instrument.go), and that a run reads its own
// redirections with to hash what it wrote. It is an allowlist:
//
//	command     = simple commands joined by "|", "&&", ";" or a newline
//	simple      = word { word | redirection }
//	word        = one or more of A-Z a-z 0-9 _ - . / , : = + @ %
//	redirection = [digits] (">" | ">>" | "<") word  |  [digits] (">&" | "<&") digits
//
// So no quotes, $, backticks, globs (* ? [ ]), ~, braces, parentheses,
// backslashes, comments, "||", "&" or here-documents.

// SimpleCommand is one simple command of a command line: its words, the
// files it reads with <, writes with > and appends to with >>, and whether
// it has any redirection.
type SimpleCommand struct {
	Words      []string
	Inputs     []string
	Outputs    []string
	Appends    []string
	Redirected bool
}

// commandWordChar is whether a word of a command line may hold c.
func commandWordChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_-./,:=+@%", c) >= 0
}

// refusedChar names the construct a character outside the grammar starts.
func refusedChar(c byte) string {
	switch c {
	case '\'', '"':
		return "quotes"
	case '$':
		return "$ (a variable or a command substitution)"
	case '`':
		return "backticks (a command substitution)"
	case '*', '?', '[', ']':
		return "a glob (" + string(c) + ")"
	case '~':
		return "~ (a home directory expansion)"
	case '{', '}':
		return "braces"
	case '(', ')':
		return "parentheses (a subshell or a process substitution)"
	case '\\':
		return "a backslash escape"
	case '#':
		return "# (a comment)"
	}
	return fmt.Sprintf("the character %q", c)
}

// ParseCommandLine splits command into simple commands under the grammar
// above, or says what it cannot read.
func ParseCommandLine(command string) ([]SimpleCommand, error) {
	var commands []SimpleCommand
	var current SimpleCommand
	// pending is a "|" or "&&" still waiting for the command after it.
	pending := ""
	end := func(op string) error {
		if len(current.Words) == 0 {
			switch {
			case current.Redirected:
				return fmt.Errorf("a redirection with no command")
			case pending != "":
				return fmt.Errorf("an empty command after %s", pending)
			case op != ";":
				return fmt.Errorf("an empty command before %s", op)
			}
			return nil
		}
		commands = append(commands, current)
		current = SimpleCommand{}
		pending = ""
		if op != ";" {
			pending = op
		}
		return nil
	}
	word := func(i int) (string, int) {
		start := i
		for i < len(command) && commandWordChar(command[i]) {
			i++
		}
		return command[start:i], i
	}
	for i := 0; i < len(command); {
		c := command[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '\n' || c == ';':
			if err := end(";"); err != nil {
				return nil, err
			}
			i++
		case c == '|':
			if strings.HasPrefix(command[i:], "||") {
				return nil, fmt.Errorf("|| (a command run on another's failure)")
			}
			if err := end("|"); err != nil {
				return nil, err
			}
			i++
		case c == '&':
			if !strings.HasPrefix(command[i:], "&&") {
				return nil, fmt.Errorf("& (a command in the background, or &>)")
			}
			if err := end("&&"); err != nil {
				return nil, err
			}
			i += 2
		case c == '<' || c == '>':
			if err := redirection(command, &i, "", &current); err != nil {
				return nil, err
			}
		case commandWordChar(c):
			w, next := word(i)
			if next < len(command) && (command[next] == '<' || command[next] == '>') && isDigits(w) {
				i = next
				if err := redirection(command, &i, w, &current); err != nil {
					return nil, err
				}
				continue
			}
			if next < len(command) && !strings.ContainsRune(" \t\n;|&<>", rune(command[next])) {
				return nil, fmt.Errorf("%s in %q", refusedChar(command[next]), w+string(command[next]))
			}
			current.Words = append(current.Words, w)
			i = next
		default:
			return nil, fmt.Errorf("%s", refusedChar(c))
		}
	}
	if err := end(";"); err != nil {
		return nil, err
	}
	if len(commands) == 0 {
		return nil, fmt.Errorf("no command")
	}
	return commands, nil
}

// redirection reads one redirection at command[*i], a < or >, into into.
func redirection(command string, i *int, fd string, into *SimpleCommand) error {
	into.Redirected = true
	rest := command[*i:]
	op := rest[:1]
	switch {
	case strings.HasPrefix(rest, "<<"):
		return fmt.Errorf("<< (a here-document)")
	case strings.HasPrefix(rest, "<>"):
		return fmt.Errorf("<> (a read-write redirection)")
	case strings.HasPrefix(rest, ">|"):
		return fmt.Errorf(">| (a clobbering redirection)")
	case strings.HasPrefix(rest, ">>"):
		op = ">>"
	}
	*i += len(op)
	if *i < len(command) && command[*i] == '&' {
		start := *i + 1
		end := start
		for end < len(command) && command[end] >= '0' && command[end] <= '9' {
			end++
		}
		if op == ">>" || end == start {
			return fmt.Errorf("%s%s& with no descriptor after it", fd, op)
		}
		*i = end
		return nil
	}
	for *i < len(command) && (command[*i] == ' ' || command[*i] == '\t') {
		*i++
	}
	start := *i
	for *i < len(command) && commandWordChar(command[*i]) {
		*i++
	}
	target := command[start:*i]
	if *i < len(command) && !strings.ContainsRune(" \t\n;|&<>", rune(command[*i])) {
		return fmt.Errorf("%s in the target of %s", refusedChar(command[*i]), op)
	}
	if target == "" {
		return fmt.Errorf("%s%s with no file after it", fd, op)
	}
	switch op {
	case "<":
		into.Inputs = append(into.Inputs, target)
	case ">":
		into.Outputs = append(into.Outputs, target)
	case ">>":
		into.Appends = append(into.Appends, target)
	}
	return nil
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// redirectHashes is a SHA-256 of each regular file command wrote with > or
// >>, resolved against dir, as the files stand now: when the run ended.
// A command outside the grammar hashes nothing.
func redirectHashes(command, dir string) map[string]string {
	commands, err := ParseCommandLine(command)
	if err != nil {
		return nil
	}
	wrote := map[string]string{}
	for _, simple := range commands {
		for _, target := range append(append([]string{}, simple.Outputs...), simple.Appends...) {
			path := target
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
			path = filepath.Clean(path)
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			content, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			sum := sha256.Sum256(content)
			wrote[path] = hex.EncodeToString(sum[:])
		}
	}
	if len(wrote) == 0 {
		return nil
	}
	return wrote
}
