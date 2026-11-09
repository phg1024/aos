package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/framing"
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
// at EstimatedCharsPerToken.
const mailBatchBytes = 2 * ipc.MaxBodyBytes

// mailBatch is what one step may take on, at least one message. It stays within
// what compaction keeps live, or the mail would be archived before any model read
// it: one place below KeepMessages leaves room for mailWaitingNote, and half of the
// target leaves room for the framing around each body. Under the default policy,
// for an unknown window, that is 2,000 bytes, so mail over it comes one message a
// step. And no new mail comes while the last batch has not reached a model: a failed
// call must not make the next request larger.
func (r *AgentRuntime) mailBatch(session Session) ipc.Batch {
	policy := r.compactionPolicy()
	batch := ipc.Batch{Bytes: mailBatchBytes, Hold: slices.ContainsFunc(session.MailboxUnshown, func(mail UnshownMail) bool { return mail.Segment == "" })}
	if policy.KeepMessages > 0 {
		batch.Messages = max(policy.KeepMessages-1, 1)
	}
	if target := policy.targetChars(); target > 0 {
		batch.Bytes = min(batch.Bytes, target/2)
	}
	return batch
}

// takeMail is the step's mail, and what the batch left behind.
func (r *AgentRuntime) takeMail(pid string, session Session, redeliver bool) ([]ipc.Message, ipc.Waiting, error) {
	if r.NoBusBounds {
		messages, err := r.Bus.Receive(pid, redeliver)
		return messages, ipc.Waiting{}, err
	}
	return r.Bus.ReceiveBatch(pid, redeliver, session.MailboxSeen, r.mailBatch(session))
}

// waitingNotes tells the model what a batch left behind, once the step's mail is in
// session. That mail is already in the mailbox when the model decides, and would
// become dead letters if it exits. The note is not mail, and it stays with the last
// turn when compaction cuts, as a tool result does. The same line with no mail after
// it adds nothing, which keeps a failing call's request the same. With mail after it
// the line is about other mail, however alike the numbers.
func waitingNotes(session Session, waiting ipc.Waiting) []Message {
	if waiting.Messages == 0 {
		return nil
	}
	note := runtimeNote("tool", framing.RuntimeNotice, mailWaitingNote(waiting))
	for i := len(session.Messages) - 1; i >= 0 && session.Messages[i].Mail == ""; i-- {
		if session.Messages[i].Content == note.Content {
			return nil
		}
	}
	return []Message{note}
}

func mailWaitingNote(waiting ipc.Waiting) string {
	unit := "messages are"
	if waiting.Messages == 1 {
		unit = "message is"
	}
	return fmt.Sprintf("%s %d more %s waiting in your mailbox, %d bytes. A step takes in only so much mail, so they arrive at your next steps. If you exit now they go unread, and their senders are told.", framing.RuntimeNotice, waiting.Messages, unit, waiting.Bytes)
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
	rights := readRights{parents: make(map[string]string, len(processes)), granted: map[string]bool{}}
	for _, process := range processes {
		byPID[process.PID] = process
		rights.parents[process.PID] = process.ParentPID
	}
	rights.chain = Chain(byPID, pid)
	for _, record := range records {
		received := record.Type == "mailbox_message" && record.Metadata["direction"] == "receive"
		if (received || record.Type == DelegationHandlesArtifact) && rights.chain[record.ProducerPID] {
			for _, id := range splitHandles(record.Metadata[handlesMetadata]) {
				rights.granted[id] = true
			}
		}
	}
	return rights, nil
}

// Chain is pid and the processes it continues, through recoveries and a new root's
// take-over. Links past maxResumeChain count for nothing, which fails closed.
func Chain(byPID map[string]kernel.Process, pid string) map[string]bool {
	chain := map[string]bool{}
	for current, hops := pid, 0; current != "" && hops < maxResumeChain && !chain[current]; hops++ {
		process, ok := byPID[current]
		if !ok {
			break
		}
		chain[current] = true
		current = process.Metadata["resumed_from"]
		if current == "" {
			current = process.Metadata[ContinuesMetadata]
		}
	}
	return chain
}

// GrantHandles records what a delegator attached, for the process delegated to. The
// runtime checked the delegator's right to each before the delegation started.
func GrantHandles(store *artifact.Store, pid, delegator string, ids []string) error {
	_, err := store.CreateJSON(DelegationHandlesArtifact, pid, map[string]any{"delegator": delegator, "artifacts": ids}, nil, map[string]string{
		"delegator":     delegator,
		handlesMetadata: strings.Join(ids, ","),
	})
	return err
}

// delegationHandles is what a delegation attaches, once the delegator's right to each
// is checked: the same check as mail. Before bounds and handles there is nothing to attach.
func (r *AgentRuntime) delegationHandles(process kernel.Process, tool, raw string) (string, error) {
	if r.NoBusBounds {
		return "", nil
	}
	ids := splitHandles(raw)
	if err := r.checkHandles(process, tool, ids); err != nil {
		return "", err
	}
	return strings.Join(ids, ","), nil
}

// AttachedNote tells a process delegated to what came attached, or "" for nothing.
func AttachedNote(store *artifact.Store, ids []string) (string, error) {
	if len(ids) == 0 {
		return "", nil
	}
	records, err := store.List()
	if err != nil {
		return "", err
	}
	var lines []string
	for _, id := range ids {
		if at := slices.IndexFunc(records, func(record artifact.Artifact) bool { return record.ID == id }); at >= 0 {
			from := "typed"
			if PublishedFromFile(records[at]) {
				from = "from a file"
			}
			lines = append(lines, fmt.Sprintf("%s (%s, %s)", id, records[at].Type, from))
		}
	}
	return "\n\nAttached to this delegation, which you may read with read_artifact: " + strings.Join(lines, ", ") + ".", nil
}

// PublishedFromFile is whether a published artifact holds a file's bytes.
func PublishedFromFile(record artifact.Artifact) bool {
	return record.Metadata[sourceMetadata] == sourceFile
}

// PublishedPath is the file a publication was read from, or "".
func PublishedPath(record artifact.Artifact) string {
	return record.Metadata[sourcePathMetadata]
}

// PublishedSHA256 is the SHA-256 of the bytes a publication read from its
// file, or "".
func PublishedSHA256(record artifact.Artifact) string {
	return record.Metadata[sourceSHA256Metadata]
}

// SplitHandles parses a comma-separated list of artifact IDs, without repeats.
func SplitHandles(raw string) []string { return splitHandles(raw) }

// ErrMisplacedArtifacts refuses an action whose artifacts the runtime will not
// guess at: given in two places that differ, or on an action that attaches nothing.
var ErrMisplacedArtifacts = errors.New("artifacts not attached")

// attaches names the actions that carry artifacts: a delegate, spawn_agent and message_agent.
func attaches(action GenerateAction) bool {
	if action.Type == "delegate" {
		return true
	}
	return action.Type == "tool" && action.ToolCall != nil && (action.ToolCall.Name == "spawn_agent" || action.ToolCall.Name == "message_agent")
}

// placeArtifacts puts what a reply attached where the action reads it: inside the
// delegate, or in spawn_agent's or message_agent's args. A reply may also put
// artifacts at the action level, or beside a tool_call's args, and means the same.
// Places that differ are refused, not one picked, and so is any artifacts on an
// action that attaches nothing. live4's coordinator put them at the action level
// twice, and nothing was attached or said.
func placeArtifacts(action *GenerateAction) error {
	outer := []string{action.Artifacts}
	if action.ToolCall != nil {
		outer = append(outer, action.ToolCall.Artifacts)
	}
	if action.Send != nil {
		outer = append(outer, action.Send.Artifacts)
	}
	if !attaches(*action) {
		for _, raw := range outer {
			if len(splitHandles(raw)) > 0 {
				return fmt.Errorf("%w: only a delegate, spawn_agent and message_agent attach artifacts, and this %s did not run; to send handles by mail, use message_agent's artifacts", ErrMisplacedArtifacts, actionName(*action))
			}
		}
		return nil
	}
	if action.Type == "delegate" && action.Delegate == nil {
		return nil // malformed, and refused as that
	}
	inner := ""
	if action.Type == "delegate" {
		inner = action.Delegate.Artifacts
	} else {
		inner = action.ToolCall.Args["artifacts"]
	}
	chosen, moved := splitHandles(inner), false
	for _, raw := range outer {
		ids := splitHandles(raw)
		if len(ids) == 0 {
			continue
		}
		if len(chosen) > 0 && !sameHandles(chosen, ids) {
			return fmt.Errorf("%w: two lists that differ, %s and %s, so this %s did not run; give artifacts once, inside %s", ErrMisplacedArtifacts,
				strings.Join(chosen, ","), strings.Join(ids, ","), actionName(*action), innerPlace(*action))
		}
		if len(chosen) == 0 {
			chosen, moved = ids, true
		}
	}
	dropPlacedArtifacts(action)
	if !moved {
		return nil
	}
	if action.Type == "delegate" {
		action.Delegate.Artifacts = strings.Join(chosen, ",")
		return nil
	}
	if action.ToolCall.Args == nil {
		action.ToolCall.Args = map[string]string{}
	}
	action.ToolCall.Args["artifacts"] = strings.Join(chosen, ",")
	return nil
}

// dropPlacedArtifacts clears the places outside the call, as a reply was read before
// delegate_attach.
func dropPlacedArtifacts(action *GenerateAction) {
	action.Artifacts = ""
	if action.ToolCall != nil {
		action.ToolCall.Artifacts = ""
	}
	if action.Send != nil {
		action.Send.Artifacts = ""
	}
}

func sameHandles(a, b []string) bool {
	return len(a) == len(b) && !slices.ContainsFunc(a, func(id string) bool { return !slices.Contains(b, id) })
}

// actionName is a tool's name, or the action's type.
func actionName(action GenerateAction) string {
	if action.Type == "tool" && action.ToolCall != nil && action.ToolCall.Name != "" {
		return action.ToolCall.Name
	}
	return action.Type
}

func innerPlace(action GenerateAction) string {
	if action.Type == "delegate" {
		return `"delegate"`
	}
	return action.ToolCall.Name + `'s args`
}

// attachedField is a delegate_result's word on what the delegation attached.
func attachedField(result DelegateResult) string {
	if result.Attached == "" {
		return ""
	}
	return " attached=" + result.Attached
}

// ContinuesMetadata names, on a new root that takes over an ended root's
// conversation, the root it continues. It is not a recovery, so mail and
// subscriptions do not follow it, only the right to read what the old root could.
const ContinuesMetadata = "continues"

// DelegationHandlesArtifact records handles attached at a delegation. Its producer
// is the process delegated to, and its handlesMetadata what the delegator attached.
const DelegationHandlesArtifact = "delegation_handles"

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
		"artifact_id":  id,
		"offset":       strconv.Itoa(offset),
		"total_bytes":  strconv.Itoa(len(text)),
		"body":         text[offset:end],
		sourceMetadata: sourceTyped,
	}
	if end < len(text) {
		output["next_offset"] = strconv.Itoa(end)
	}
	if r.NoPublishSource {
		delete(output, sourceMetadata)
	} else if at := slices.IndexFunc(records, func(record artifact.Artifact) bool { return record.ID == id }); records[at].Metadata[sourceMetadata] == sourceFile {
		output[sourceMetadata] = sourceFile
		output[sourcePathMetadata] = records[at].Metadata[sourcePathMetadata]
		output[sourceSHA256Metadata] = records[at].Metadata[sourceSHA256Metadata]
	}
	return tools.Result{Tool: "read_artifact", Summary: "read artifact", Output: output}, nil
}

// What a published artifact's bytes came from, on its record and in what a reader
// is told: the file named at the publish, as read then, or what the model typed. The
// runtime vouches for the bytes, and not for how the file came to hold them.
const (
	sourceMetadata       = "source"
	sourcePathMetadata   = "source_path"
	sourceSHA256Metadata = "source_sha256"
	sourceFile           = "file"
	sourceTyped          = "typed"
)

// publicationBody is what a publish stores, and the record's metadata: the body the
// model typed, or the bytes of a file it names. A file adds no authority: the
// process must hold read_file and be able to read that path with it.
func (r *AgentRuntime) publicationBody(ctx context.Context, process kernel.Process, call ToolCall) (string, map[string]string, error) {
	metadata := map[string]string{"kind": "published"}
	if r.NoPublishSource {
		return call.Args["body"], metadata, nil
	}
	path := strings.TrimSpace(call.Args["path"])
	body, typed := call.Args["body"]
	switch {
	case path == "" && !typed:
		return "", nil, errors.New("publish_artifact requires body or path")
	case path == "":
		return body, metadata, nil
	case body != "":
		return "", nil, errors.New("publish_artifact takes body or path, not both")
	}
	if !(capability.Evaluator{}).ToolAllowed("read_file", process.CapabilitySet) {
		r.logDenial(process.PID, call.Name, "read_file", path, "capability not granted")
		return "", nil, fmt.Errorf("%w: publish_artifact path needs read_file", tools.ErrToolDenied)
	}
	content, err := r.Tools.ReadSource(ctx, tools.Request{
		Tool:          call.Name,
		Args:          call.Args,
		CapabilitySet: process.CapabilitySet,
		Mounts:        process.Mounts,
		PID:           process.PID,
	})
	if errors.Is(err, tools.ErrPathDenied) {
		r.logDenial(process.PID, call.Name, "read_file", path, tools.DenialReason(err))
	}
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(content)
	metadata[sourceMetadata] = sourceFile
	metadata[sourcePathMetadata] = path
	metadata[sourceSHA256Metadata] = hex.EncodeToString(sum[:])
	if !r.NoPublishProvenance {
		for key, value := range r.provenance(process, path) {
			metadata[key] = value
		}
	}
	return string(content), metadata, nil
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
