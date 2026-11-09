package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"aos/internal/artifact"
	"aos/internal/kernel"
	"aos/internal/statedir"
	"aos/internal/tools"
)

// What a publication from a file records about what produced the file: the
// latest run of the publishing process whose command names the file, and a
// SHA-256 of each file that command named: by absolute path, or relative to
// the directory run starts in (the process's first mount). A reader can
// then tell a record a trusted instrument wrote from one a worker's script
// wrote. It is a record of what was run, not a proof: a command that runs
// a file without naming it (make reading its Makefile) is recorded only by
// what it names.
const (
	// ProducedByMetadata is the producing run's tool_result ID, or "none".
	ProducedByMetadata = "produced_by"
	// ProducedByCommandMetadata is that run's command.
	ProducedByCommandMetadata = "produced_by_command"
	// ProducedByRanMetadata lists "path=sha256" for each file the command
	// names, other than the published one, joined by newlines.
	ProducedByRanMetadata = "produced_by_ran"
	// ProducedByNone is ProducedByMetadata when no run named the file.
	ProducedByNone = "none"
	// maxProvenanceCommand bounds the recorded command.
	maxProvenanceCommand = 2000
)

// provenance is the producing run of path, published by process, as
// metadata.
func (r *AgentRuntime) provenance(process kernel.Process, path string) map[string]string {
	pid, dir := process.PID, ""
	if len(process.Mounts) > 0 {
		dir = process.Mounts[0].HostPath
	}
	path = filepath.Clean(path)
	out := map[string]string{ProducedByMetadata: ProducedByNone}
	records, err := r.Artifacts.List()
	if err != nil {
		return out
	}
	var latest *artifact.Artifact
	var command string
	for i := range records {
		record := &records[i]
		if record.Type != "tool_result" || record.ProducerPID != pid || record.Metadata["tool"] != "run" {
			continue
		}
		if latest != nil && !statedir.Newer(record.ID, latest.ID) {
			continue
		}
		var result tools.Result
		if err := r.Artifacts.ReadJSON(record.ID, &result); err != nil || !slices.Contains(commandPaths(result.Output["command"], dir), path) {
			continue
		}
		latest, command = record, result.Output["command"]
	}
	if latest == nil {
		return out
	}
	out[ProducedByMetadata] = latest.ID
	if len(command) > maxProvenanceCommand {
		command = command[:maxProvenanceCommand]
	}
	out[ProducedByCommandMetadata] = command
	if ran := RanFiles(command, path, dir); len(ran) > 0 {
		lines := make([]string, 0, len(ran))
		for _, file := range sortedKeys(ran) {
			lines = append(lines, file+"="+ran[file])
		}
		out[ProducedByRanMetadata] = strings.Join(lines, "\n")
	}
	return out
}

// commandPaths are the paths command's words name: its words, with quotes
// and redirections taken off, relative ones joined to dir (none when dir is
// empty).
func commandPaths(command, dir string) []string {
	var paths []string
	for _, word := range strings.FieldsFunc(command, func(c rune) bool {
		return c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '|' || c == '&' || c == '(' || c == ')'
	}) {
		word = strings.Trim(word, `"'`)
		word = strings.TrimLeft(word, "0123456789<>")
		word = strings.Trim(word, `"'`)
		switch {
		case word == "" || strings.HasPrefix(word, "-"):
		case filepath.IsAbs(word):
			paths = append(paths, filepath.Clean(word))
		case dir != "":
			paths = append(paths, filepath.Join(dir, word))
		}
	}
	return paths
}

// RanFiles hashes each regular file command names (commandPaths, relative to
// dir), other than skip.
func RanFiles(command, skip, dir string) map[string]string {
	ran := map[string]string{}
	for _, word := range commandPaths(command, dir) {
		if word == filepath.Clean(skip) {
			continue
		}
		info, err := os.Stat(word)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		content, err := os.ReadFile(word)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(content)
		ran[word] = hex.EncodeToString(sum[:])
	}
	return ran
}

// ParseRan reads ProducedByRanMetadata back into paths and hashes.
func ParseRan(value string) map[string]string {
	ran := map[string]string{}
	for _, line := range strings.Split(value, "\n") {
		if path, sum, ok := strings.Cut(line, "="); ok && path != "" {
			ran[path] = sum
		}
	}
	return ran
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
