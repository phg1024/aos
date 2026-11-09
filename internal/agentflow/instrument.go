package agentflow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"aos/internal/tools"
)

// The instrument rules (run.started instrument_rules) decide whether a
// record declared trusted_instrument has a trusted instrument behind it. A
// record is trusted only when the engine can account for everything its
// producing command ran, and every file it accounts for is trusted or lies
// where no agent can write. Anything the engine cannot read is refused, with
// the construct that stopped it as the reason: the grammar is an allowlist,
// so a route nobody thought of fails closed rather than passing.
//
// The engine reads the command with tools.ParseCommandLine's grammar:
// simple commands joined by |, &&, ; or a newline, words of A-Z a-z 0-9
// _ - . / , : = + @ %, and file redirections; so no quotes, $, backticks,
// globs, ~, braces, parentheses, backslashes, comments, ||, & or
// here-documents. Then each simple command must hold:
//
//   - It does not start with an assignment (NAME=value), which can change
//     what the program finds (PATH=...).
//   - Its program is not one that changes the shell or runs a command line
//     of its own (shellNames, wrapperNames: cd, ., source, eval, exec, env,
//     xargs, timeout, make, npm and the like), and find has no -exec.
//   - Its program resolves: a word with a slash names a file, which must
//     exist; a bare name, other than echo, printf, true, false, test and :,
//     is looked up on PATH, and every PATH directory searched up to it must
//     be absolute and lie where no agent can write.
//   - An interpreter (sh, python3, node and others: interpreters) is given
//     only the options listed for it, then a script named as an existing
//     file: no -c, -m, -e or standard input. sed and awk take their program
//     from a file (-f), since an inline one can run commands.
//   - Every argument and input that names a path is accounted for: a word
//     with a slash always (a file by its hash, anything else as "-"), a bare
//     word when it exists relative to the working directory. An option with
//     a path in it must be -name=path. The published file itself may be
//     named as an argument, but not read with < or appended to with >>,
//     whose earlier content no instrument accounts for.
//
// Version 2 binds the record to the run (instrumentAccounted's bind): the
// command must write the published file with >, and the published bytes
// must hash to what the run left when it ended (tools.Result.Wrote). A tool
// that writes its record by naming it as an argument is refused: a run
// hashes only its redirections, and a file it merely names may be one a
// worker wrote before it. A record changed after its run, by anyone, is
// refused.
//
// The rules see one command line. A trusted script, or a system program,
// that itself runs or reads a file a worker can write is trusted
// transitively: what a program does once it runs is not in the command.
// Files are read when the verdict is reached, not when the command ran.

// InstrumentNoFile is the account of a named path that is not a regular
// file: a directory, a device, or nothing.
const InstrumentNoFile = "-"

// shellNames change the shell's own state, or are its syntax: a command
// after them is not read the way the grammar reads it.
var shellNames = setOf("cd", "pushd", "popd", ".", "source", "eval", "exec", "alias", "unalias", "export", "set", "unset",
	"readonly", "local", "declare", "typeset", "trap", "command", "builtin", "hash", "enable", "shopt", "read", "mapfile",
	"readarray", "let", "function", "time", "coproc", "ulimit", "umask", "getopts", "shift", "fc", "history",
	"if", "then", "else", "elif", "fi", "for", "while", "until", "do", "done", "case", "esac", "select", "in", "!")

// wrapperNames run a command line of their own, given as arguments or found
// in the working directory (make's Makefile, npm's package.json).
var wrapperNames = setOf("env", "xargs", "nohup", "timeout", "gtimeout", "nice", "renice", "sudo", "doas", "su", "stdbuf",
	"chroot", "watch", "parallel", "script", "flock", "setsid", "caffeinate", "arch", "unbuffer", "taskset", "numactl",
	"chrt", "ionice", "strace", "ltrace", "gdb", "lldb", "valgrind", "sandbox-exec", "open", "osascript", "busybox",
	"make", "gmake", "ninja", "just", "npm", "npx", "yarn", "pnpm", "srun", "mpirun", "mpiexec", "ssh", "expect")

// shellBuiltins run without a PATH lookup and run nothing else.
var shellBuiltins = setOf("echo", "printf", "true", "false", "test", ":")

// interpreters run a script, and the single-letter options each may take
// before it. A version suffix (python3.11) is dropped before the lookup.
var interpreters = map[string]string{
	"sh": "eux", "bash": "eux", "zsh": "eux", "dash": "eux", "ksh": "eux", "mksh": "eux",
	"python": "BEIOSsuq", "pypy": "BEIOSsuq",
	"perl": "w", "ruby": "w", "node": "", "nodejs": "", "deno": "", "bun": "",
	"lua": "", "luajit": "", "php": "", "rscript": "", "julia": "", "tclsh": "", "pwsh": "", "fish": "", "csh": "", "tcsh": "",
}

// codeFromFile run inline programs that can start commands (sed's e, awk's
// system), so they must take their program from a file with -f.
var codeFromFile = setOf("sed", "gsed", "awk", "gawk", "mawk", "nawk")

var versionSuffix = regexp.MustCompile(`[0-9.]+$`)

func setOf(names ...string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}

// instrumentAccount is what command ran, as the grammar reads it: each
// path it names or runs, by SHA-256 for a regular file and InstrumentNoFile
// otherwise. dir is where the command ran, pathEnv the PATH it searched,
// published the record's file, and writable whether a worker can write a
// path. An error is the reason the command cannot be accounted for.
func instrumentAccount(command, dir, pathEnv, published string, writable func(string) bool) (map[string]string, error) {
	commands, err := tools.ParseCommandLine(command)
	if err != nil {
		return nil, err
	}
	a := instrumentAccounting{dir: dir, pathEnv: pathEnv, writable: writable, ran: map[string]string{}}
	a.published = a.abs(published)
	for _, simple := range commands {
		if err := a.simple(simple); err != nil {
			return nil, err
		}
	}
	return a.ran, nil
}

type instrumentAccounting struct {
	dir, pathEnv, published string
	writable                func(string) bool
	ran                     map[string]string
}

func (a *instrumentAccounting) simple(c tools.SimpleCommand) error {
	program := c.Words[0]
	if name, _, ok := strings.Cut(program, "="); ok && isIdentifier(name) {
		return fmt.Errorf("a leading assignment (%s=...), which can change what the program finds", name)
	}
	base := filepath.Base(program)
	switch {
	case shellNames[base]:
		return fmt.Errorf("%s, which changes the shell or is its syntax", base)
	case wrapperNames[base]:
		return fmt.Errorf("%s, which runs a command line of its own", base)
	}
	if strings.Contains(program, "/") || !shellBuiltins[base] {
		path, err := a.resolve(program)
		if err != nil {
			return err
		}
		if err := a.account(path, true); err != nil {
			return err
		}
	}
	args := c.Words[1:]
	interpreter, isInterpreter := interpreters[strings.ToLower(versionSuffix.ReplaceAllString(base, ""))]
	switch {
	case base == "find":
		for _, arg := range args {
			switch arg {
			case "-exec", "-execdir", "-ok", "-okdir":
				return fmt.Errorf("find %s, which runs a command line of its own", arg)
			}
		}
	case codeFromFile[base]:
		file := false
		for _, arg := range args {
			switch {
			case arg == "-f":
				file = true
			case arg == "-e" || strings.HasPrefix(arg, "--expression") || strings.HasPrefix(arg, "--source"):
				return fmt.Errorf("%s %s, an inline program, which can run commands", base, arg)
			}
		}
		if !file {
			return fmt.Errorf("%s with an inline program, which can run commands: give it one with -f", base)
		}
	case isInterpreter:
		script, err := interpreterScript(base, interpreter, args)
		if err != nil {
			return err
		}
		path := a.abs(script)
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("%s's script %s is not a file", base, script)
		}
	}
	for _, arg := range args {
		candidate := arg
		if strings.HasPrefix(arg, "-") {
			if !strings.Contains(arg, "/") {
				continue
			}
			_, value, ok := strings.Cut(arg, "=")
			if !ok {
				return fmt.Errorf("an option with a path joined to it (%s): write -name=path", arg)
			}
			candidate = value
		} else if name, value, ok := strings.Cut(arg, "="); ok && isIdentifier(name) {
			candidate = value
		}
		if candidate == "" {
			continue
		}
		if err := a.account(a.abs(candidate), strings.Contains(candidate, "/")); err != nil {
			return err
		}
	}
	for _, input := range c.Inputs {
		path := a.abs(input)
		if path == a.published {
			return fmt.Errorf("< %s reads the published file, whose earlier content no instrument accounts for", input)
		}
		if err := a.account(path, true); err != nil {
			return err
		}
	}
	for _, target := range c.Appends {
		if a.abs(target) == a.published {
			return fmt.Errorf(">> %s appends to the published file, whose earlier content no instrument accounts for", target)
		}
	}
	return nil
}

// interpreterScript is the script an interpreter's arguments name, after
// the options allowed is given.
func interpreterScript(base, allowed string, args []string) (string, error) {
	for i, arg := range args {
		switch {
		case arg == "-":
			return "", fmt.Errorf("%s -, which reads its program from standard input", base)
		case arg == "--":
			if i+1 < len(args) {
				return args[i+1], nil
			}
		case strings.HasPrefix(arg, "-"):
			flags := arg[1:]
			for _, flag := range flags {
				if !strings.ContainsRune(allowed, flag) || flag == '-' {
					return "", fmt.Errorf("%s %s, %s", base, arg, interpreterOption(flag, allowed))
				}
			}
		default:
			return arg, nil
		}
	}
	return "", fmt.Errorf("%s with no script file, which reads its program from standard input", base)
}

func interpreterOption(flag rune, allowed string) string {
	switch flag {
	case 'c':
		return "which runs a command line given as an argument"
	case 'm':
		return "which runs a module found on its search path"
	case 'e', 'p':
		return "which runs code given as an argument"
	}
	if allowed == "" {
		return "an option: none may come before the script"
	}
	return "an option outside -" + allowed + ", the only ones that may come before the script"
}

func isIdentifier(s string) bool {
	if s == "" || s[0] >= '0' && s[0] <= '9' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// abs is word as a path, relative ones joined to the working directory.
func (a *instrumentAccounting) abs(word string) string {
	if filepath.IsAbs(word) || a.dir == "" {
		return filepath.Clean(word)
	}
	return filepath.Join(a.dir, word)
}

// resolve finds program: by its path when it has a slash, else on PATH,
// where every directory searched up to it must lie where no worker writes.
func (a *instrumentAccounting) resolve(program string) (string, error) {
	if strings.Contains(program, "/") {
		path := a.abs(program)
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("program %s is not a file", program)
		}
		return path, nil
	}
	for _, dir := range filepath.SplitList(a.pathEnv) {
		if dir == "" || !filepath.IsAbs(dir) {
			return "", fmt.Errorf("PATH holds a relative directory (%q), searched in the working directory, before %s is found", dir, program)
		}
		if a.writable(dir) {
			return "", fmt.Errorf("PATH searches %s, where a worker can write, before %s is found", dir, program)
		}
		path := filepath.Join(dir, program)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s is not on PATH", program)
}

// account records path: a regular file by its hash, and anything else as
// InstrumentNoFile when always is set or it exists. The published file is
// left out: a tool may name the record it writes.
func (a *instrumentAccounting) account(path string, always bool) error {
	if path == a.published {
		return nil
	}
	info, err := os.Stat(path)
	switch {
	case err == nil && info.Mode().IsRegular():
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%s cannot be read: %v", path, err)
		}
		sum := sha256.Sum256(content)
		a.ran[path] = hex.EncodeToString(sum[:])
	case err == nil || always:
		a.ran[path] = InstrumentNoFile
	}
	return nil
}

// writesByRedirect is whether command writes published with >, resolved
// against dir.
func writesByRedirect(command, dir, published string) bool {
	commands, err := tools.ParseCommandLine(command)
	if err != nil {
		return false
	}
	a := instrumentAccounting{dir: dir}
	published = a.abs(published)
	for _, simple := range commands {
		for _, target := range simple.Outputs {
			if a.abs(target) == published {
				return true
			}
		}
	}
	return false
}
