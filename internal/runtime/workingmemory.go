package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"aos/internal/artifact"
	"aos/internal/tools"
)

// MaxWorkingMemoryBytes caps a rendered working memory. An update over it
// is refused, never truncated, so the model never sees cut text as its own.
const MaxWorkingMemoryBytes = 8 * 1024

// maxMemoryKeyBytes bounds an entry's key: a key names a subject.
const maxMemoryKeyBytes = 80

// MemorySections are working memory's sections, in the order they show.
var MemorySections = []string{"decisions", "measurements", "rejected", "open", "next"}

// evidenceSections are the sections whose entries should say what they
// rest on; the snapshot marks those that do not.
var evidenceSections = map[string]bool{"decisions": true, "rejected": true}

// MemoryEntry is one keyed claim. Evidence is what the agent says it rests
// on: message numbers or artifact IDs the runtime checked resolve.
type MemoryEntry struct {
	Key      string `json:"key"`
	Content  string `json:"content"`
	Evidence string `json:"evidence,omitempty"`
}

// MemorySection is one section's entries, in the order they were added.
type MemorySection struct {
	Name    string        `json:"name"`
	Entries []MemoryEntry `json:"entries,omitempty"`
}

// WorkingMemory is one version of a process's working memory: the payload
// of a working_memory artifact, whose parent is the previous version.
type WorkingMemory struct {
	ProcessID string          `json:"process_id"`
	Version   int             `json:"version"`
	Sections  []MemorySection `json:"sections"`
	Update    MemoryUpdate    `json:"update"`
	// ArtifactID and Written are filled when a version is read back.
	ArtifactID string    `json:"-"`
	Written    time.Time `json:"-"`
}

// MemoryUpdate is the change that made a version.
type MemoryUpdate struct {
	Section  string `json:"section"`
	Key      string `json:"key"`
	Delete   bool   `json:"delete,omitempty"`
	Content  string `json:"content,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

// Entry returns the entry under section and key, if any.
func (m WorkingMemory) Entry(section, key string) (MemoryEntry, bool) {
	for _, s := range m.Sections {
		if s.Name != section {
			continue
		}
		for _, entry := range s.Entries {
			if entry.Key == key {
				return entry, true
			}
		}
	}
	return MemoryEntry{}, false
}

// Render is working memory as the prompt shows it, and what its cap counts.
func (m WorkingMemory) Render() string {
	var builder strings.Builder
	for _, section := range m.Sections {
		if len(section.Entries) == 0 {
			continue
		}
		builder.WriteString(section.Name + ":\n")
		for _, entry := range section.Entries {
			builder.WriteString(renderEntry(section.Name, entry) + "\n")
		}
	}
	return builder.String()
}

func renderEntry(section string, entry MemoryEntry) string {
	line := "- " + entry.Key + ": " + entry.Content
	switch {
	case entry.Evidence != "":
		line += " [evidence " + entry.Evidence + "]"
	case evidenceSections[section]:
		line += " [no evidence]"
	}
	return line
}

// sectionSizes reports each section's rendered size, for an over-cap error.
func (m WorkingMemory) sectionSizes() string {
	var sizes []string
	for _, section := range m.Sections {
		size := 0
		for _, entry := range section.Entries {
			size += len(renderEntry(section.Name, entry)) + 1
		}
		if size > 0 {
			size += len(section.Name) + 2
		}
		sizes = append(sizes, fmt.Sprintf("%s %d", section.Name, size))
	}
	return strings.Join(sizes, ", ")
}

// apply returns m with update made, as the next version.
func (m WorkingMemory) apply(update MemoryUpdate) WorkingMemory {
	next := WorkingMemory{ProcessID: m.ProcessID, Version: m.Version + 1, Update: update}
	for _, name := range MemorySections {
		section := MemorySection{Name: name}
		for _, old := range m.Sections {
			if old.Name == name {
				section.Entries = append(section.Entries, old.Entries...)
			}
		}
		if name == update.Section {
			replaced := false
			entries := section.Entries[:0:0]
			for _, entry := range section.Entries {
				if entry.Key != update.Key {
					entries = append(entries, entry)
					continue
				}
				replaced = true
				if !update.Delete {
					entries = append(entries, MemoryEntry{Key: update.Key, Content: update.Content, Evidence: update.Evidence})
				}
			}
			if !replaced && !update.Delete {
				entries = append(entries, MemoryEntry{Key: update.Key, Content: update.Content, Evidence: update.Evidence})
			}
			section.Entries = entries
		}
		next.Sections = append(next.Sections, section)
	}
	return next
}

// readWorkingMemory reads one version.
func (r *AgentRuntime) readWorkingMemory(id string) (WorkingMemory, error) {
	record, err := r.Artifacts.Metadata(id)
	if err != nil {
		return WorkingMemory{}, err
	}
	if record.Type != "working_memory" {
		return WorkingMemory{}, fmt.Errorf("%s is a %s, not working memory", id, record.Type)
	}
	var memory WorkingMemory
	if err := r.Artifacts.ReadJSON(id, &memory); err != nil {
		return WorkingMemory{}, err
	}
	memory.ArtifactID = id
	memory.Written = record.CreatedAt
	return memory, nil
}

// workingMemoryChain reads every version from the first to id.
func (r *AgentRuntime) workingMemoryChain(id string) ([]WorkingMemory, error) {
	var chain []WorkingMemory
	for id != "" && len(chain) < 100000 {
		memory, err := r.readWorkingMemory(id)
		if err != nil {
			return nil, err
		}
		chain = append([]WorkingMemory{memory}, chain...)
		record, err := r.Artifacts.Metadata(id)
		if err != nil {
			return nil, err
		}
		id = ""
		if len(record.ParentIDs) > 0 {
			id = record.ParentIDs[0]
		}
	}
	return chain, nil
}

var evidenceRange = regexp.MustCompile(`^(\d+)(?:-(\d+))?$`)

// checkEvidence refuses evidence that does not resolve: a message range
// outside the caller's numbered history, or an artifact that does not exist
// or is another process's memory or checkpoint.
func (r *AgentRuntime) checkEvidence(pid string, session Session, evidence string) (string, error) {
	var parts []string
	for _, part := range strings.Split(evidence, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		parts = append(parts, part)
		if m := evidenceRange.FindStringSubmatch(part); m != nil {
			from, _ := strconv.Atoi(m[1])
			to := from
			if m[2] != "" {
				to, _ = strconv.Atoi(m[2])
			}
			last := numberMessages(session).NextSeq
			if from < 1 || to < from || to > last {
				return "", fmt.Errorf("evidence %q is not in your history: its messages are numbered 1-%d", part, last)
			}
			continue
		}
		record, err := r.Artifacts.Metadata(part)
		if err != nil {
			return "", fmt.Errorf("evidence %q is neither a message range such as 1180-1214 nor an artifact: %w", part, err)
		}
		switch record.Type {
		case "memory_segment", "checkpoint", "working_memory":
			if err := r.ownedBy(pid, record); err != nil {
				return "", fmt.Errorf("evidence %s is another process's %s", part, record.Type)
			}
		}
	}
	return strings.Join(parts, ", "), nil
}

// updateMemory sets or deletes one keyed entry and writes the result as the
// next working_memory version. A replay of an update an earlier attempt
// already wrote adopts that version instead of writing a second one.
func (r *AgentRuntime) updateMemory(pid string, session Session, args map[string]string) (tools.Result, error) {
	section := strings.TrimSpace(args["section"])
	if !contains(MemorySections, section) {
		return tools.Result{}, fmt.Errorf("update_memory section must be one of %s", strings.Join(MemorySections, ", "))
	}
	key := strings.TrimSpace(args["key"])
	if key == "" || len(key) > maxMemoryKeyBytes || strings.ContainsAny(key, "\n\r") {
		return tools.Result{}, fmt.Errorf("update_memory key must be one line of at most %d bytes naming the subject", maxMemoryKeyBytes)
	}
	update := MemoryUpdate{Section: section, Key: key}
	update.Delete, _ = strconv.ParseBool(strings.TrimSpace(args["delete"]))
	if !update.Delete {
		update.Content = strings.Join(strings.Fields(args["content"]), " ")
		if update.Content == "" {
			return tools.Result{}, errors.New("update_memory needs content, or delete=true")
		}
		evidence, err := r.checkEvidence(pid, session, args["evidence"])
		if err != nil {
			return tools.Result{}, fmt.Errorf("update_memory: %w", err)
		}
		update.Evidence = evidence
	}

	current := WorkingMemory{ProcessID: pid}
	if session.WorkingMemory != "" {
		var err error
		if current, err = r.readWorkingMemory(session.WorkingMemory); err != nil {
			return tools.Result{}, err
		}
	}
	if update.Delete {
		if _, ok := current.Entry(section, key); !ok {
			return tools.Result{}, fmt.Errorf("update_memory: no entry %s/%s to delete", section, key)
		}
	}
	next := current.apply(update)
	next.ProcessID = pid
	if size := len(next.Render()); size > MaxWorkingMemoryBytes {
		entry := len(renderEntry(section, MemoryEntry{Key: key, Content: update.Content, Evidence: update.Evidence}))
		return tools.Result{}, fmt.Errorf("update_memory: working memory would be %d bytes, over its %d-byte cap; now: %s; this entry: %d bytes. Shorten or delete entries, then retry",
			size, MaxWorkingMemoryBytes, current.sectionSizes(), entry)
	}

	idempotency := updateKey(session.WorkingMemory, update)
	record, err := r.adoptMemoryVersion(pid, idempotency)
	if err != nil {
		return tools.Result{}, err
	}
	adopted := record.ID != ""
	if !adopted {
		var parents []string
		if session.WorkingMemory != "" {
			parents = []string{session.WorkingMemory}
		}
		record, err = r.Artifacts.CreateJSON("working_memory", pid, next, parents, map[string]string{
			"version":    strconv.Itoa(next.Version),
			"update_key": idempotency,
		})
		if err != nil {
			return tools.Result{}, err
		}
	}
	op := "set"
	if update.Delete {
		op = "deleted"
	}
	return tools.Result{
		Tool:    "update_memory",
		Summary: "updated working memory",
		Output: map[string]string{
			"artifact_id": record.ID,
			"version":     strconv.Itoa(next.Version),
			"entry":       op + " " + section + "/" + key,
			"bytes":       fmt.Sprintf("%d of %d", len(next.Render()), MaxWorkingMemoryBytes),
			"adopted":     strconv.FormatBool(adopted),
		},
	}, nil
}

// updateKey identifies one update to one version: a replay of the same
// update to the same parent has the same key.
func updateKey(parent string, update MemoryUpdate) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%t\x00%s\x00%s", parent, update.Section, update.Key, update.Delete, update.Content, update.Evidence)))
	return hex.EncodeToString(sum[:])
}

// adoptMemoryVersion finds a version this process's chain already wrote for
// the same update to the same parent: the effect of a step that crashed
// before its checkpoint recorded it.
func (r *AgentRuntime) adoptMemoryVersion(pid, key string) (artifact.Artifact, error) {
	records, err := r.Artifacts.List()
	if err != nil {
		return artifact.Artifact{}, err
	}
	for _, record := range records {
		if record.Type != "working_memory" || record.Metadata["update_key"] != key {
			continue
		}
		if r.ownedBy(pid, record) == nil {
			return record, nil
		}
	}
	return artifact.Artifact{}, nil
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
