package agentflow_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/agentflow"
)

// instrumentWorld is a trusted directory, a worker's directory with its
// own bin, and a system bin holding the programs the commands run.
type instrumentWorld struct {
	trusted, work, workBin, system, record string
}

func newInstrumentWorld(t *testing.T) instrumentWorld {
	t.Helper()
	root := t.TempDir()
	w := instrumentWorld{trusted: filepath.Join(root, "trusted"), work: filepath.Join(root, "work"), system: filepath.Join(root, "system")}
	w.workBin, w.record = filepath.Join(w.work, "bin"), filepath.Join(w.work, "raw.txt")
	for _, dir := range []string{w.trusted, w.workBin, w.system} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]os.FileMode{
		filepath.Join(w.trusted, "bench.sh"): 0o644, filepath.Join(w.trusted, "bench.py"): 0o644, filepath.Join(w.trusted, "prog"): 0o755,
		filepath.Join(w.trusted, "parse.awk"): 0o644, filepath.Join(w.work, "bench.sh"): 0o644, filepath.Join(w.work, "input.txt"): 0o644,
		filepath.Join(w.workBin, "bench"): 0o755,
	}
	for _, name := range []string{"sh", "python3", "python3.11", "cat", "find", "sed", "awk", "env", "xargs", "cd", "node", "tool", "bench"} {
		files[filepath.Join(w.system, name)] = 0o755
	}
	for path, mode := range files {
		if err := os.WriteFile(path, []byte("content of "+filepath.Base(path)+"\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func (w instrumentWorld) account(command, pathEnv string) (map[string]string, error) {
	command = strings.NewReplacer("TRUSTED", w.trusted, "WORK", w.work, "RECORD", w.record).Replace(command)
	return agentflow.InstrumentAccount(command, w.work, pathEnv, w.record, func(path string) bool {
		path = filepath.Clean(path)
		return path == w.work || strings.HasPrefix(path, w.work+string(filepath.Separator))
	})
}

// The grammar refuses every route that reaches a file without naming it,
// and says which construct it refused.
func TestInstrumentGrammarRefuses(t *testing.T) {
	w := newInstrumentWorld(t)
	for _, c := range []struct{ command, reason string }{
		{"cd WORK && sh bench.sh > RECORD", "cd, which changes the shell"},
		{"S=WORK/bench.sh; sh $S > RECORD", "$ (a variable"},
		{"S=WORK/bench.sh sh TRUSTED/bench.sh > RECORD", "a leading assignment (S=...)"},
		{"sh WORK/*.sh > RECORD", "a glob (*)"},
		{"sh WORK/bench.s? > RECORD", "a glob (?)"},
		{"sh WORK/bench.[s]h > RECORD", "a glob ([)"},
		{"sh -c ls > RECORD", "sh -c, which runs a command line given as an argument"},
		{"sh -c 'sh WORK/bench.sh' > RECORD", "quotes"},
		{"sh \"WORK/bench.sh\" > RECORD", "quotes"},
		{"sh $(echo WORK/bench.sh) > RECORD", "$ (a variable or a command substitution)"},
		{"sh `echo WORK/bench.sh` > RECORD", "backticks"},
		{". ./bench.sh > RECORD", "., which changes the shell"},
		{"source WORK/bench.sh > RECORD", "source, which changes the shell"},
		{"eval sh > RECORD", "eval, which changes the shell"},
		{"exec sh TRUSTED/bench.sh > RECORD", "exec, which changes the shell"},
		{"python3 -m bench > RECORD", "python3 -m, which runs a module found on its search path"},
		{"python3.11 -c print > RECORD", "python3.11 -c, which runs a command line"},
		{"node -e x > RECORD", "node -e, which runs code given as an argument"},
		{"find WORK -name bench.sh -exec sh + > RECORD", "find -exec, which runs a command line of its own"},
		{"find WORK -exec sh {} ; > RECORD", "braces"},
		{"echo WORK | xargs sh > RECORD", "xargs, which runs a command line of its own"},
		{"env -S sh > RECORD", "env, which runs a command line of its own"},
		{"env sh TRUSTED/bench.sh > RECORD", "env, which runs a command line of its own"},
		{"PATH=WORK/bin:/usr/bin bench > RECORD", "a leading assignment (PATH=...)"},
		{"make bench > RECORD", "make, which runs a command line of its own"},
		{"sh < WORK/bench.sh > RECORD", "sh with no script file, which reads its program from standard input"},
		{"cat WORK/bench.sh | sh > RECORD", "sh with no script file"},
		{"sh - < TRUSTED/bench.sh > RECORD", "sh -, which reads its program from standard input"},
		{"sh -s TRUSTED/bench.sh > RECORD", "sh -s, an option outside -eux"},
		{"sh TRUSTED/bench.sh || sh WORK/bench.sh > RECORD", "|| (a command run on another's failure)"},
		{"sh TRUSTED/bench.sh > RECORD &", "& (a command in the background"},
		{"sh TRUSTED/bench.sh &> RECORD", "& (a command in the background"},
		{"sh TRUSTED/bench.sh >> RECORD", ">> " + "WORK/raw.txt appends to the published file"},
		{"cat < RECORD > RECORD", "reads the published file"},
		{"sh <<EOF > RECORD", "<< (a here-document)"},
		{"sh ~/bench.sh > RECORD", "~ (a home directory expansion)"},
		{"sh WORK/{bench}.sh > RECORD", "braces"},
		{"(sh WORK/bench.sh) > RECORD", "parentheses"},
		{"sh WORK/bench\\.sh > RECORD", "a backslash escape"},
		{"sh TRUSTED/bench.sh # WORK/bench.sh", "# (a comment)"},
		{"sed s/a/b/ TRUSTED/bench.sh > RECORD", "sed with an inline program"},
		{"awk -f TRUSTED/parse.awk -e x TRUSTED/bench.sh > RECORD", "awk -e, an inline program"},
		{"tool -fWORK/bench.sh > RECORD", "an option with a path joined to it"},
		{"| sh TRUSTED/bench.sh > RECORD", "an empty command before |"},
		{"sh TRUSTED/bench.sh |", "an empty command after |"},
		{"> RECORD", "a redirection with no command"},
		{"sh TRUSTED/missing.sh > RECORD", "sh's script TRUSTED/missing.sh is not a file"},
		{"WORK/missing/tool > RECORD", "program WORK/missing/tool is not a file"},
		{"nosuch > RECORD", "nosuch is not on PATH"},
		{"", "no command"},
	} {
		_, err := w.account(c.command, w.system)
		want := strings.NewReplacer("TRUSTED", w.trusted, "WORK", w.work).Replace(c.reason)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err %v, want %q", c.command, err, want)
		}
	}
}

// A program found on PATH is accounted for only when every directory
// searched up to it is absolute and out of a worker's reach.
func TestInstrumentPathLookup(t *testing.T) {
	w := newInstrumentWorld(t)
	for _, c := range []struct{ pathEnv, reason string }{
		{w.workBin + ":" + w.system, "PATH searches " + w.workBin + ", where a worker can write, before bench is found"},
		{".:" + w.system, `PATH holds a relative directory (".")`},
		{":" + w.system, `PATH holds a relative directory ("")`},
	} {
		if _, err := w.account("bench > RECORD", c.pathEnv); err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("PATH %q: err %v, want %q", c.pathEnv, err, c.reason)
		}
	}
	// A worker's directory after the program's own is never searched.
	ran, err := w.account("bench > RECORD", w.system+":"+w.workBin)
	if err != nil || len(ran[filepath.Join(w.system, "bench")]) != 64 || len(ran) != 1 {
		t.Fatalf("ran %v, err %v", ran, err)
	}
}

// What the grammar accepts it accounts for: the programs, scripts, inputs
// and named paths, by hash for a file. Whether each is trusted is the
// caller's to say.
func TestInstrumentAccountsForWhatRan(t *testing.T) {
	w := newInstrumentWorld(t)
	file := func(path string) string { return filepath.Join(w.system, path) }
	for _, c := range []struct {
		command string
		want    []string
		noFile  []string
	}{
		{"sh TRUSTED/bench.sh > RECORD", []string{file("sh"), w.trusted + "/bench.sh"}, nil},
		{"sh -eu TRUSTED/bench.sh 2>&1 | cat > RECORD; cat RECORD", []string{file("sh"), file("cat"), w.trusted + "/bench.sh"}, nil},
		{"python3 -u TRUSTED/bench.py --n 3 --out=RECORD > WORK/log.txt", []string{file("python3"), w.trusted + "/bench.py"}, nil},
		{"TRUSTED/prog WORK/input.txt < TRUSTED/bench.sh > RECORD", []string{w.trusted + "/prog", w.work + "/input.txt", w.trusted + "/bench.sh"}, nil},
		{"sh bench.sh > RECORD", []string{file("sh"), w.work + "/bench.sh"}, nil},
		{"tool --script=WORK/bench.sh --dir WORK/gone > RECORD", []string{file("tool"), w.work + "/bench.sh"}, []string{w.work + "/gone"}},
		{"tool bin > RECORD", []string{file("tool")}, []string{w.workBin}},
		{"awk -f TRUSTED/parse.awk TRUSTED/bench.sh > RECORD", []string{file("awk"), w.trusted + "/parse.awk", w.trusted + "/bench.sh"}, nil},
		// A bare word that names a file in the working directory is
		// accounted for, whatever the program makes of it.
		{"find TRUSTED -name bench.sh > RECORD", []string{file("find"), w.work + "/bench.sh"}, []string{w.trusted}},
		{"echo frame_ms=1 > RECORD", nil, nil},
	} {
		ran, err := w.account(c.command, w.system)
		if err != nil {
			t.Errorf("%q: %v", c.command, err)
			continue
		}
		if len(ran) != len(c.want)+len(c.noFile) {
			t.Errorf("%q: ran %v", c.command, ran)
		}
		for _, path := range c.want {
			if len(ran[path]) != 64 {
				t.Errorf("%q: %s not hashed in %v", c.command, path, ran)
			}
		}
		for _, path := range c.noFile {
			if ran[path] != agentflow.InstrumentNoFile {
				t.Errorf("%q: %s not accounted as no file in %v", c.command, path, ran)
			}
		}
	}
}
