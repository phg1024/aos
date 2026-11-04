package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"aos/internal/artifact"
	"aos/internal/framing"
	"aos/internal/kernel"
	"aos/internal/tools"
)

// ToolStartedEvent is the intent a world tool call records in the kernel
// log before it acts. The call's outcome artifact (tool_result, tool_error,
// tool_cancelled) names the intent's event ID in its "intent" metadata. An
// intent no outcome names is a call that started and never reported back:
// the process died during it. Whoever resumes or recovers that process
// records a tool_interrupted outcome for it and tells the agent, so the
// effect is not repeated blindly.
const ToolStartedEvent = "tool.started"

// Outcome artifact types for a call cut off mid-flight: cancelled while the
// run was alive, or interrupted when the process died.
const (
	ToolCancelledType   = "tool_cancelled"
	ToolInterruptedType = "tool_interrupted"
)

// errIntentNotRecorded fails the run: a world tool never runs without its
// intent on record.
var errIntentNotRecorded = errors.New("tool intent not recorded")

// WorldTool reports whether a tool acts outside the kernel's own records:
// it runs a command or writes a file. Repeating one may repeat its effect,
// so these record an intent first. Reads and the runtime's own tools
// (mail, leases, artifacts, memory) do not.
func WorldTool(name string) bool {
	switch name {
	case "run", "write_file", "edit_file":
		return true
	}
	return false
}

// ToolIntent is a world tool call that started, from its tool.started
// event.
type ToolIntent struct {
	EventID string    `json:"event_id"`
	PID     string    `json:"pid"`
	Tool    string    `json:"tool"`
	Target  string    `json:"target,omitempty"`
	Time    time.Time `json:"time"`
}

// maxIntentTarget bounds the command or path an intent records.
const maxIntentTarget = 200

// recordIntent records that call is about to act, unless the runtime
// replays a record made before intents existed. It returns the event ID.
func (r *AgentRuntime) recordIntent(pid string, call ToolCall) (string, error) {
	if r.NoToolIntents || !WorldTool(call.Name) {
		return "", nil
	}
	args, err := json.Marshal(call.Args)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errIntentNotRecorded, err)
	}
	digest := sha256.Sum256(args)
	data := map[string]string{"tool": call.Name, "args_sha256": hex.EncodeToString(digest[:])}
	if target := intentTarget(call); target != "" {
		data["target"] = target
	}
	event, err := r.Kernel.RecordEvent(pid, ToolStartedEvent, data)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errIntentNotRecorded, err)
	}
	return event.ID, nil
}

// intentTarget is what the call acts on: the command for run, else the
// path, clipped.
func intentTarget(call ToolCall) string {
	target := call.Args["path"]
	if call.Name == "run" {
		target = call.Args["command"]
	}
	target = strings.TrimSpace(target)
	if len(target) > maxIntentTarget {
		target = target[:maxIntentTarget] + "..."
	}
	return target
}

// OpenIntents returns the intents no outcome artifact names, in log order:
// calls that started and never reported back.
func OpenIntents(events []kernel.Event, artifacts []artifact.Artifact) []ToolIntent {
	resolved := map[string]bool{}
	for _, record := range artifacts {
		if id := record.Metadata["intent"]; id != "" {
			resolved[id] = true
		}
	}
	var open []ToolIntent
	for _, event := range events {
		if event.Type == ToolStartedEvent && !resolved[event.ID] {
			open = append(open, ToolIntent{EventID: event.ID, PID: event.ProcessID, Tool: event.Data["tool"], Target: event.Data["target"], Time: event.Timestamp})
		}
	}
	sort.SliceStable(open, func(i, j int) bool { return open[i].EventID < open[j].EventID })
	return open
}

// noteInterrupted tells the agent in session, before its next step, of
// the calls its process chain started and never finished: the intents of
// previousPID and of the processes it was recovered from that no outcome
// but tool_interrupted answers. previousPID is pid itself on a resume, or
// the process it recovers. The chain, not previousPID alone, because a
// Recover whose handover failed leaves a successor that exited before it
// was told, and its predecessor, superseded, is never recovered again.
//
// Each intent gets one tool_interrupted record, produced by pid the first
// time; a later note reuses it. The note goes into session unless session
// already holds that record, so it reaches the history the agent continues
// once: a successor that failed before checkpointing its note exits at the
// checkpoint it was recovered from, and the next one is told again.
func (r *AgentRuntime) noteInterrupted(previousPID, pid, checkpointID string, session *Session) error {
	lineage, err := r.lineage(previousPID)
	if err != nil {
		return err
	}
	chain := map[string]bool{}
	for _, ancestor := range lineage {
		chain[ancestor] = true
	}
	events, err := r.Kernel.Events()
	if err != nil {
		return err
	}
	artifacts, err := r.Artifacts.List()
	if err != nil {
		return err
	}
	answered := map[string]bool{}
	told := map[string]string{}
	for _, record := range artifacts {
		if id := record.Metadata["intent"]; id != "" {
			if record.Type == ToolInterruptedType {
				told[id] = record.ID
			} else {
				answered[id] = true
			}
		}
	}
	held := map[string]bool{}
	for _, id := range session.ToolArtifactIDs {
		held[id] = true
	}
	for _, event := range events {
		if event.Type != ToolStartedEvent || !chain[event.ProcessID] || answered[event.ID] {
			continue
		}
		intent := ToolIntent{EventID: event.ID, PID: event.ProcessID, Tool: event.Data["tool"], Target: event.Data["target"], Time: event.Timestamp}
		recordID, recorded := told[intent.EventID]
		if recorded && held[recordID] {
			continue
		}
		if !recorded {
			record, err := r.Artifacts.CreateJSON(ToolInterruptedType, pid, map[string]string{
				"tool":   intent.Tool,
				"intent": intent.EventID,
				"pid":    intent.PID,
				"target": intent.Target,
			}, nonEmpty(checkpointID), map[string]string{"tool": intent.Tool, "intent": intent.EventID})
			if err != nil {
				return err
			}
			recordID = record.ID
		}
		session.ToolArtifactIDs = append(session.ToolArtifactIDs, recordID)
		session.Messages = append(session.Messages, runtimeNote("tool", framing.ToolInterrupted, interruptedMessage(intent)))
	}
	return nil
}

func interruptedMessage(intent ToolIntent) string {
	target := ""
	if intent.Target != "" {
		target = fmt.Sprintf(" target=%q", intent.Target)
	}
	return fmt.Sprintf("tool_interrupted name=%s%s started_at=%s note=this call started before the process stopped and has no recorded outcome; it may have taken effect, fully or in part; check before repeating it",
		intent.Tool, target, intent.Time.UTC().Format(time.RFC3339))
}

// cancelledNote is what a cancelled world tool call adds to its
// tool_cancelled message: that it may have acted, and the files it changed.
func cancelledNote(changes *tools.FileChanges) string {
	return fmt.Sprintf(" started=true may_have_taken_effect=true changes=%q", changeSummary(changes))
}

// changeSummary lists the workspace files a command changed, or why they
// are unknown.
func changeSummary(changes *tools.FileChanges) string {
	if changes == nil {
		return "not recorded"
	}
	if changes.Unknown != "" {
		return "unknown: " + changes.Unknown
	}
	var parts []string
	for _, group := range []struct {
		name  string
		paths []string
	}{{"created", changes.Created}, {"modified", changes.Modified}, {"deleted", changes.Deleted}} {
		if len(group.paths) > 0 {
			parts = append(parts, group.name+" "+strings.Join(group.paths, ", "))
		}
	}
	if changes.More > 0 {
		parts = append(parts, fmt.Sprintf("%d more", changes.More))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

// toolErrorPayload is a tool_error artifact's payload. A command cut off
// by its timeout adds the files it changed.
func toolErrorPayload(tool string, err error, changes *tools.FileChanges) map[string]any {
	payload := map[string]any{"tool": tool, "error": err.Error()}
	if changes != nil {
		payload["changes"] = changes
	}
	return payload
}

// outcomeMetadata is an outcome artifact's metadata: its tool, and the
// intent it answers when one was recorded.
func outcomeMetadata(tool, intent string) map[string]string {
	metadata := map[string]string{"tool": tool}
	if intent != "" {
		metadata["intent"] = intent
	}
	return metadata
}
