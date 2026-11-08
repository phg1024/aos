package agentflow

import (
	"errors"
	"strings"

	"aos/internal/runtime"
)

// How ReadReply found the JSON object it read.
const (
	// ExtractedAsSent: the reply, trimmed, was the object.
	ExtractedAsSent = "as sent"
	// ExtractedTrailing: the object was the reply's last complete JSON
	// object, after prose or other objects.
	ExtractedTrailing = "trailing object"
	// ExtractedLoose: the object needed the loose repair: the span from
	// the first brace to the last, with bare or half-quoted keys quoted and
	// trailing commas dropped.
	ExtractedLoose = "loose repair"
	// ExtractedAmbiguous: with the reply checks, the reply held two
	// different actions, and none was read.
	ExtractedAmbiguous = "two actions"
)

// ReplyReading is how the parser reads one model reply, rung by rung, for
// tools that classify recorded replies the way a step reads them.
type ReplyReading struct {
	// Extraction is how the object was found; Shape is the shape that read
	// it, empty when none did.
	Extraction string
	Shape      string
	// Usable is whether a step takes the action as it is. A message without
	// text is read but not usable: the step asks for a repair.
	Usable bool
	Result runtime.GenerateResult
}

// ReplyReader reads recorded replies as a step of a given record read
// them: NoReplyChecks for a record without reply_checks (ReplyChecksField).
type ReplyReader struct {
	NoReplyChecks bool
}

// ReaderFor is the reader for a run whose run.started data is started.
func ReaderFor(started map[string]string) ReplyReader {
	return ReplyReader{NoReplyChecks: started[ReplyChecksField] == ""}
}

// ReadReply reads raw as one step's first parse does now.
func ReadReply(raw string) ReplyReading { return ReplyReader{}.Read(raw) }

// ResolveRecordedReply is ReplyReader.Resolve as steps read replies now.
func ResolveRecordedReply(raw, repaired string, hadRepair bool) (runtime.GenerateResult, string, error) {
	return ReplyReader{}.Resolve(raw, repaired, hadRepair)
}

// Read reads raw as one step's first parse does.
func (r ReplyReader) Read(raw string) ReplyReading {
	checks := !r.NoReplyChecks
	text := raw
	if checks {
		text = stripThink(raw)
	}
	extracted := extractTrailingJSONObject(text)
	object := repairLooseJSONObject(extracted)
	reading := ReplyReading{Extraction: ExtractedAsSent}
	switch {
	case object != extracted:
		reading.Extraction = ExtractedLoose
	case object != strings.TrimSpace(text):
		reading.Extraction = ExtractedTrailing
	}
	if checks && len(distinctActions(text)) > 1 {
		reading.Extraction = ExtractedAmbiguous
		return reading
	}
	result, shape, ok := parseActionShape(object, checks)
	reading.Shape, reading.Result = shape, result
	reading.Usable = ok && !(result.Action.Type == "message" && strings.TrimSpace(result.Action.Message) == "")
	return reading
}

// Resolve runs the tolerance ladder over a recorded reply and, when the
// step asked for one, its recorded repair, and names the rung that gave the
// action (SourceReply ... SourceUnrepairable). hadRepair false means no
// repair call was made or recorded.
func (r ReplyReader) Resolve(raw, repaired string, hadRepair bool) (runtime.GenerateResult, string, error) {
	return resolveReply(raw, !r.NoReplyChecks, func() (string, error) {
		if !hadRepair {
			return "", errNoRepairRecorded
		}
		return repaired, nil
	})
}

var errNoRepairRecorded = errors.New("no repair recorded")
