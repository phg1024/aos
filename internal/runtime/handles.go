package runtime

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/tools"
)

// ErrArtifactNotReadable is one answer for an ID that does not exist, a runtime
// record, and another process's artifact, so a caller cannot use it to find out
// which IDs exist. The kernel's log has the true reason.
var ErrArtifactNotReadable = errors.New("no artifact this process may read has this ID")

// mailBatchBytes is the most mail one step takes into its request: two bodies at
// the cap. It is less than a tool output because of what is left above the
// compaction trigger when mail arrives: the smallest window
// CompactionPolicy.Validate admits, at the default ratio of 0.8, has 34 KiB there
// at EstimatedCharsPerToken. Bodies count, and framing adds to them.
const mailBatchBytes = 2 * ipc.MaxBodyBytes

// mailBatch is what one step may take, at least one message: no more than
// compaction would keep live, or it archives the mail before any model reads it,
// and no more than fits above the trigger. The rest stays pending for the next
// step. So a full mailbox cannot fail every request by landing in one, and it
// takes one step per batch.
func (r *AgentRuntime) mailBatch() ipc.Batch {
	if r.NoBusBounds {
		return ipc.Batch{}
	}
	policy := r.compactionPolicy()
	batch := ipc.Batch{Messages: policy.KeepMessages, Bytes: mailBatchBytes}
	if target := policy.targetChars(); target > 0 {
		batch.Bytes = min(batch.Bytes, target)
	}
	return batch
}

// readRights is what one process may read. A process may read a published
// artifact if its own chain produced it, if it was attached to mail the chain
// received, or if a descendant of the chain published it. Rights only grow: a
// rollback undoes no receipt, so a grant lasts as long as the chain.
type readRights struct {
	chain   map[string]bool
	parents map[string]string
	granted map[string]bool
}

// readRightsOf reads the rights of pid from the kernel and the given records.
// It walks back through recoveries, and past a new root that continues an ended
// one's conversation: that root holds its handles in its history. Both links
// point backward, so it is enough to start from a live process. Links past
// maxResumeChain count for nothing, which fails closed.
func (r *AgentRuntime) readRightsOf(pid string, records []artifact.Artifact) (readRights, error) {
	processes, err := r.Kernel.Processes()
	if err != nil {
		return readRights{}, err
	}
	byPID := make(map[string]kernel.Process, len(processes))
	rights := readRights{chain: map[string]bool{}, parents: make(map[string]string, len(processes)), granted: map[string]bool{}}
	for _, process := range processes {
		byPID[process.PID] = process
		rights.parents[process.PID] = process.ParentPID
	}
	for current, hops := pid, 0; current != "" && hops < maxResumeChain && !rights.chain[current]; hops++ {
		process, ok := byPID[current]
		if !ok {
			break
		}
		rights.chain[current] = true
		current = process.Metadata["resumed_from"]
		if current == "" {
			current = process.Metadata[ContinuesMetadata]
		}
	}
	for _, record := range records {
		if record.Type == "mailbox_message" && record.Metadata["direction"] == "receive" && rights.chain[record.ProducerPID] {
			for _, id := range splitHandles(record.Metadata[handlesMetadata]) {
				rights.granted[id] = true
			}
		}
	}
	return rights, nil
}

// ContinuesMetadata names, on a new root that takes over an ended root's
// conversation, the root it continues. It is not a recovery, so mail and
// subscriptions do not follow it, only the right to read what the old root could.
const ContinuesMetadata = "continues"

// handlesMetadata holds, on a receive's mailbox_message record, the handles the
// message carried. The runtime writes it from what it checked at the send.
const handlesMetadata = "artifacts"

// why says why record may not be read, or "" if it may.
func (rights readRights) why(record artifact.Artifact) string {
	switch {
	case !strings.HasPrefix(record.Type, artifact.PublishedPrefix):
		return "runtime record"
	case rights.chain[record.ProducerPID], rights.granted[record.ID]:
		return ""
	}
	for current, hops := rights.parents[record.ProducerPID], 0; current != "" && hops < maxResumeChain; hops++ {
		if rights.chain[current] {
			return ""
		}
		current = rights.parents[current]
	}
	return "not its chain's, not attached to its mail, not a descendant's"
}

// splitHandles parses a comma-separated list of artifact IDs, without repeats.
func splitHandles(raw string) []string {
	var ids []string
	for _, id := range strings.Split(raw, ",") {
		if id = strings.TrimSpace(id); id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// checkHandles refuses a handle the sender cannot read itself: it passes on only
// what it holds, as delegation does.
func (r *AgentRuntime) checkHandles(process kernel.Process, tool string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	records, err := r.Artifacts.List()
	if err != nil {
		return err
	}
	rights, err := r.readRightsOf(process.PID, records)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := r.mayRead(process.PID, tool, id, records, rights); err != nil {
			return err
		}
	}
	return nil
}

// mayRead is the one check on an ID. A refusal reads the same whatever its
// reason, and is logged with the reason.
func (r *AgentRuntime) mayRead(pid, tool, id string, records []artifact.Artifact, rights readRights) error {
	reason := "no such artifact"
	if at := slices.IndexFunc(records, func(record artifact.Artifact) bool { return record.ID == id }); at >= 0 {
		reason = rights.why(records[at])
	}
	if reason == "" {
		return nil
	}
	r.logDenial(pid, tool, "read_artifact", "", id+": "+reason)
	return fmt.Errorf("%w: %q", ErrArtifactNotReadable, id)
}

// readArtifact returns one page of a published artifact's body. A page is at most
// a tool output, and is cut on a rune: it backs up to a rune's start, and always
// moves forward, by a whole rune past a limit too small for one. Every read goes
// through the store's hash check, so a damaged payload is a tool error.
func (r *AgentRuntime) readArtifact(pid string, args map[string]string) (tools.Result, error) {
	id := strings.TrimSpace(args["id"])
	if id == "" {
		id = strings.TrimSpace(args["artifact_id"])
	}
	if id == "" {
		return tools.Result{}, errors.New("read_artifact requires id")
	}
	offset, err := byteArg(args["offset"], 0, 0)
	if err != nil {
		return tools.Result{}, fmt.Errorf("read_artifact offset must be a non-negative byte offset: %w", err)
	}
	limit, err := byteArg(args["limit"], tools.MaxToolOutputBytes, 1)
	if err != nil {
		return tools.Result{}, fmt.Errorf("read_artifact limit must be a positive number of bytes: %w", err)
	}
	limit = min(limit, tools.MaxToolOutputBytes)
	records, err := r.Artifacts.List()
	if err != nil {
		return tools.Result{}, err
	}
	rights, err := r.readRightsOf(pid, records)
	if err != nil {
		return tools.Result{}, err
	}
	if err := r.mayRead(pid, "read_artifact", id, records, rights); err != nil {
		return tools.Result{}, err
	}
	var payload struct {
		Body string `json:"body"`
	}
	if err := r.Artifacts.ReadJSON(id, &payload); err != nil {
		return tools.Result{}, fmt.Errorf("read_artifact %s: %w", id, err)
	}
	text := payload.Body
	offset = min(offset, len(text))
	for offset > 0 && offset < len(text) && !utf8.RuneStart(text[offset]) {
		offset--
	}
	end := len(text)
	if limit < len(text)-offset {
		end = offset + limit
		for end > offset && !utf8.RuneStart(text[end]) {
			end--
		}
		if end == offset {
			_, size := utf8.DecodeRuneInString(text[offset:])
			end = offset + size
		}
	}
	output := map[string]string{
		"artifact_id": id,
		"offset":      strconv.Itoa(offset),
		"total_bytes": strconv.Itoa(len(text)),
		"body":        text[offset:end],
	}
	if end < len(text) {
		output["next_offset"] = strconv.Itoa(end)
	}
	return tools.Result{Tool: "read_artifact", Summary: "read artifact", Output: output}, nil
}

// byteArg parses a numeric tool argument, with fallback when absent. A number too
// large to parse counts as the largest, which every caller then clamps.
func byteArg(raw string, fallback, least int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(raw, "-") {
		return int(^uint(0) >> 1), nil
	}
	if err != nil {
		return 0, err
	}
	if n < least {
		return 0, fmt.Errorf("got %d", n)
	}
	return n, nil
}

// artifactReference stands in for a read_artifact result in a memory segment: the
// artifact stays readable, and pages copied into segments would store it twice.
func artifactReference(id, offset, end, total string) string {
	return fmt.Sprintf("read_artifact result (%s, bytes %s-%s of %s): not copied into this segment; read_artifact id=%s offset=%s reads it again", id, offset, end, total, id, offset)
}

// mayChange reports a refusal the same call can get past later: a mailbox drains,
// and mail can bring the handle a read lacked. Repeating such a call is what the
// refusal asks for, so it does not count towards the repeated-call suppression.
func mayChange(err error) bool {
	return errors.Is(err, ipc.ErrMailboxFull) || errors.Is(err, ErrArtifactNotReadable)
}

// withRemedy adds to a bus's refusal what this process can do about it, which the
// bus cannot say: it does not know what the sender holds. A handle goes in
// message_agent's artifacts, which every sender holds, since sending needs it. An
// ID in a body grants nothing, so the text says where it must go, and it names
// publish_artifact only to a process that may call it.
func (r *AgentRuntime) withRemedy(process kernel.Process, err error, publishing bool) error {
	canPublish := (capability.Evaluator{}).ToolAllowed("publish_artifact", process.CapabilitySet)
	var remedy string
	switch {
	case errors.Is(err, ipc.ErrMailboxFull):
		remedy = "its reader has not caught up: wait and send again, or send less"
	case !errors.Is(err, ipc.ErrMessageTooLarge):
		return err
	case publishing && canPublish:
		remedy = "a publish carries no handles: shorten it, or publish_artifact it and message_agent each reader with the artifact_id in artifacts (an ID in a body grants nothing)"
	case publishing:
		remedy = "a publish carries no handles, and this process cannot publish_artifact: shorten it"
	case canPublish:
		remedy = "publish_artifact it, then message_agent with the artifact_id in artifacts (an ID in the body grants nothing)"
	default:
		remedy = "this process cannot publish_artifact: send less, or several shorter messages"
	}
	return fmt.Errorf("%w; %s", err, remedy)
}
