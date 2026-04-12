package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"aos/internal/app"
)

type fakeCompleter struct {
	call int
}

func (f *fakeCompleter) CompleteJSON(_ context.Context, _ string, _ string, target any) error {
	f.call++
	if !setField(target, "Content", "combined document") {
		if !setField(target, "Python", proceduralPython()) {
			if !setField(target, "Reason", "looks good") || !setBoolField(target, "Approved", true) {
				return errors.New("unexpected target")
			}
		}
	}
	return nil
}

type rejectingCompleter struct{}

func (rejectingCompleter) CompleteJSON(_ context.Context, _ string, _ string, target any) error {
	if !setField(target, "Content", "candidate") {
		if !setField(target, "Python", proceduralPython()) {
			if !setField(target, "Reason", "missing details") || !setBoolField(target, "Approved", false) {
				return errors.New("unexpected target")
			}
		}
	}
	return nil
}

func proceduralPython() string {
	return "import argparse\n" +
		"parser = argparse.ArgumentParser()\n" +
		"parser.add_argument('inputs', nargs='+')\n" +
		"parser.add_argument('--output', required=True)\n" +
		"args = parser.parse_args()\n" +
		"parts = []\n" +
		"for path in args.inputs:\n" +
		"    with open(path, 'r', encoding='utf-8') as fh:\n" +
		"        parts.append(fh.read())\n" +
		"with open(args.output, 'w', encoding='utf-8') as out:\n" +
		"    out.write('\\n--\\n'.join(parts))\n"
}

func setField(target any, name, value string) bool {
	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return false
	}
	field := v.Elem().FieldByName(name)
	if !field.IsValid() || !field.CanSet() || field.Kind() != reflect.String {
		return false
	}
	field.SetString(value)
	return true
}

func setBoolField(target any, name string, value bool) bool {
	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return false
	}
	field := v.Elem().FieldByName(name)
	if !field.IsValid() || !field.CanSet() || field.Kind() != reflect.Bool {
		return false
	}
	field.SetBool(value)
	return true
}

func TestAssembleFilesWritesVerifiedOutput(t *testing.T) {
	root := t.TempDir()
	inputA := filepath.Join(root, "A.txt")
	inputB := filepath.Join(root, "B.txt")
	output := filepath.Join(root, "out", "D.txt")

	if err := os.WriteFile(inputA, []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write input A: %v", err)
	}
	if err := os.WriteFile(inputB, []byte("beta"), 0o644); err != nil {
		t.Fatalf("write input B: %v", err)
	}

	result, err := app.AssembleFiles(context.Background(), &fakeCompleter{}, app.AssembleRequest{
		Goal:       "assemble A and B into D",
		InputPaths: []string{inputA, inputB},
		OutputPath: output,
	})
	if err != nil {
		t.Fatalf("assemble files: %v", err)
	}
	if result.SourceCount != 2 {
		t.Fatalf("expected 2 sources, got %d", result.SourceCount)
	}
	bytes, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(bytes) != "combined document" {
		t.Fatalf("unexpected output %q", string(bytes))
	}
}

func TestAssembleFilesFailsWhenVerifierRejects(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "A.txt")
	output := filepath.Join(root, "D.txt")
	if err := os.WriteFile(input, []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}

	_, err := app.AssembleFiles(context.Background(), rejectingCompleter{}, app.AssembleRequest{
		Goal:       "assemble A into D",
		InputPaths: []string{input},
		OutputPath: output,
	})
	if err == nil || !errors.Is(err, app.ErrVerificationFailed) {
		t.Fatalf("expected verification failure, got %v", err)
	}
	if _, statErr := os.Stat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected no output file to be written, got %v", statErr)
	}
}

func TestAssembleFilesProceduralModeRunsGeneratedPython(t *testing.T) {
	root := t.TempDir()
	inputA := filepath.Join(root, "A.txt")
	inputB := filepath.Join(root, "B.txt")
	output := filepath.Join(root, "combined.txt")

	if err := os.WriteFile(inputA, []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write input A: %v", err)
	}
	if err := os.WriteFile(inputB, []byte("beta"), 0o644); err != nil {
		t.Fatalf("write input B: %v", err)
	}

	result, err := app.AssembleFiles(context.Background(), &fakeCompleter{}, app.AssembleRequest{
		Goal:       "Write a python program and run it to combine A and B into one file",
		InputPaths: []string{inputA, inputB},
		OutputPath: output,
	})
	if err != nil {
		t.Fatalf("assemble files procedural: %v", err)
	}
	if result.Mode != "procedural" {
		t.Fatalf("expected procedural mode, got %q", result.Mode)
	}
	if result.ScriptPath == "" {
		t.Fatal("expected script path to be returned")
	}
	bytes, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(bytes) != "alpha\n--\nbeta" {
		t.Fatalf("unexpected procedural output %q", string(bytes))
	}
}
