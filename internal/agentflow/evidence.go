package agentflow

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"aos/internal/artifact"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/statedir"
	"aos/internal/tools"
)

// ExitRefusedEvent records a verifier's exit 0 refused for evidence it had not read.
const ExitRefusedEvent = "verification.exit_refused"

// ErrEvidenceUnread is what a verifier reads when its exit 0 is refused.
var ErrEvidenceUnread = errors.New("exit 0 refused")

// What a verifier's chain has done with a declared record. reports/ipc-live/analyze.py
// computes the same from the same records, and a test holds them together.
const (
	EvidenceRead         = "read"
	EvidencePartial      = "partial"
	EvidenceUnread       = "unread"
	EvidenceNotPublished = "not_published"
	EvidenceNotFromFile  = "not_from_file"
)

// EvidenceCheck is one declared record at one moment: which artifact it resolved
// to, if any, and what the verifier's chain has done with it.
type EvidenceCheck struct {
	Publisher  string `json:"publisher"`
	Type       string `json:"type"`
	ArtifactID string `json:"artifact_id,omitempty"`
	Status     string `json:"status"`
}

// evidenceChecks resolves each declared record to the latest match under the
// verifier's workflow root, and reads what the verifier's own chain did with it.
// It goes by the kernel's process table and the artifact store's records of tool
// results, none of which a model writes, so it needs nothing from a model.
func (e *Engine) evidenceChecks(spec WorkflowSpec, verifierPID string) ([]EvidenceCheck, error) {
	declared := spec.Verification.Evidence
	processes, err := e.Kernel.Processes()
	if err != nil {
		return nil, err
	}
	records, err := e.Artifacts.List()
	if err != nil {
		return nil, err
	}
	byPID := make(map[string]kernel.Process, len(processes))
	for _, process := range processes {
		byPID[process.PID] = process
	}
	chain := runtime.Chain(byPID, verifierPID)
	root := rootOf(byPID, verifierPID)
	checks := make([]EvidenceCheck, 0, len(declared))
	for _, evidence := range declared {
		check := EvidenceCheck{Publisher: evidence.Publisher, Type: evidence.Type, Status: EvidenceNotPublished}
		var latest *artifact.Artifact
		for i := range records {
			record := &records[i]
			if record.Type != artifact.PublishedPrefix+evidence.Type || byPID[record.ProducerPID].Metadata["agent_id"] != evidence.Publisher || rootOf(byPID, record.ProducerPID) != root {
				continue
			}
			if latest == nil || statedir.Newer(record.ID, latest.ID) {
				latest = record
			}
		}
		if latest != nil {
			check.ArtifactID = latest.ID
			if evidence.FromFile && !runtime.PublishedFromFile(*latest) {
				check.Status = EvidenceNotFromFile
			} else {
				pages, err := e.readPages(records, chain, latest.ID)
				if err != nil {
					return nil, err
				}
				switch {
				case len(pages) == 0:
					check.Status = EvidenceUnread
				case readWhole(pages):
					check.Status = EvidenceRead
				default:
					check.Status = EvidencePartial
				}
			}
		}
		checks = append(checks, check)
	}
	return checks, nil
}

func rootOf(byPID map[string]kernel.Process, pid string) string {
	for hops := 0; hops < 1<<16; hops++ {
		parent := byPID[pid].ParentPID
		if parent == "" {
			return pid
		}
		pid = parent
	}
	return ""
}

// readPage is one read_artifact result.
type readPage struct {
	offset, end int
	last        bool
}

// readPages lists what chain read of id, from the results the runtime recorded.
func (e *Engine) readPages(records []artifact.Artifact, chain map[string]bool, id string) ([]readPage, error) {
	var pages []readPage
	for _, record := range records {
		if record.Type != "tool_result" || record.Metadata["tool"] != "read_artifact" || !chain[record.ProducerPID] {
			continue
		}
		var result tools.Result
		if err := e.Artifacts.ReadJSON(record.ID, &result); err != nil {
			return nil, err
		}
		if result.Output["artifact_id"] != id {
			continue
		}
		offset, err := strconv.Atoi(result.Output["offset"])
		if err != nil {
			return nil, fmt.Errorf("read_artifact result %s: %w", record.ID, err)
		}
		page := readPage{offset: offset, last: result.Output["next_offset"] == ""}
		if !page.last {
			if page.end, err = strconv.Atoi(result.Output["next_offset"]); err != nil {
				return nil, fmt.Errorf("read_artifact result %s: %w", record.ID, err)
			}
		}
		pages = append(pages, page)
	}
	return pages, nil
}

// readWhole is whether pages join up from the start to the end: the last page alone
// is not a reading. analyze.py's read_whole is the same rule.
func readWhole(pages []readPage) bool {
	slices.SortStableFunc(pages, func(a, b readPage) int {
		switch {
		case a.offset != b.offset:
			return a.offset - b.offset
		case a.last != b.last:
			if a.last {
				return 1
			}
			return -1
		}
		return a.end - b.end
	})
	reach := 0
	for _, page := range pages {
		if page.offset > reach {
			return false
		}
		if page.last {
			return true
		}
		reach = max(reach, page.end)
	}
	return false
}

// withEvidence applies a declared record to a verdict reached from exits. A pass needs
// the record read, and a verifier the runtime ended judged nothing. A verifier's own
// failure stands whatever it read, as it does without a declaration, so that running
// the verifier again cannot shop for a pass. Staleness comes first: the verifier has to
// run again anyway. A verdict that was never a judgement stays as it is.
func (e *Engine) withEvidence(spec WorkflowSpec, verdict Verdict) (Verdict, error) {
	if len(spec.Verification.Evidence) == 0 || verdict.VerifierPID == "" {
		return verdict, nil
	}
	checks, err := e.evidenceChecks(spec, verdict.VerifierPID)
	if err != nil {
		return Verdict{}, err
	}
	verdict.Evidence = checks
	unmet := unmetEvidence(checks)
	switch {
	case verdict.Status == VerdictPass && unmet != "":
		verdict.Status = VerdictUnverified
		verdict.Reason = fmt.Sprintf("verifier %s exited 0, and %s. This says nothing about the work: verification is what is missing", verdict.VerifierPID, unmet)
	case verdict.Status == VerdictFail && verdict.VerifierExit.Origin != kernel.ExitOriginAgent:
		verdict.Status = VerdictUnverified
		verdict.Reason = fmt.Sprintf("verifier %s ended without a judgement (%s). This says nothing about the work: verification is what is missing", verdict.VerifierPID, verdict.VerifierExit.Reason)
	}
	return verdict, nil
}

// unmetEvidence says what is still missing, or "" if nothing is.
func unmetEvidence(checks []EvidenceCheck) string {
	var unmet []string
	for _, check := range checks {
		label := fmt.Sprintf("%s (%s%s, the latest from %s)", check.ArtifactID, artifact.PublishedPrefix, check.Type, check.Publisher)
		switch check.Status {
		case EvidenceUnread:
			unmet = append(unmet, label+" was not read")
		case EvidencePartial:
			unmet = append(unmet, label+" was read only in part")
		case EvidenceNotFromFile:
			unmet = append(unmet, label+" is not from a file, and a file is required")
		case EvidenceNotPublished:
			unmet = append(unmet, fmt.Sprintf("%s has published no %s", check.Publisher, check.Type))
		}
	}
	return strings.Join(unmet, "; ")
}

// exitCheck is the runtime's check on an exit 0 by agent: nil unless agent is the
// verifier of a workflow that declares evidence. A refusal is a tool error the model
// can act on while it has steps. It cannot be the only guard, since a process can run
// out of steps and a later record can arrive after an accepted exit, so judge applies
// the same check to whatever exit it reads.
func (e *Engine) exitCheck(spec WorkflowSpec, agent AgentSpec) func(kernel.Process) error {
	if spec.Verification == nil || agent.ID != spec.Verification.Verifier || len(spec.Verification.Evidence) == 0 {
		return nil
	}
	return func(process kernel.Process) error {
		checks, err := e.evidenceChecks(spec, process.PID)
		if err != nil {
			return err
		}
		unmet := unmetEvidence(checks)
		if unmet == "" {
			return nil
		}
		data := map[string]string{"unmet": unmet}
		for _, check := range checks {
			data[check.Type] = strings.TrimSpace(check.ArtifactID + " " + check.Status)
		}
		if _, err := e.Kernel.RecordEvent(process.PID, ExitRefusedEvent, data); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s. Read a published record to its end with read_artifact in this process: what another process read does not count. If you may not read it or it is not there, use wait and say what you need, and delegating to you again can attach it; do not exit with a failure for that. A process that ends without it is recorded as unverified, which says nothing about the work", ErrEvidenceUnread, unmet)
	}
}

// evidencePrompt tells a declared verifier or publisher what the verdict rests on.
func evidencePrompt(spec WorkflowSpec, agentID string) string {
	if spec.Verification == nil || len(spec.Verification.Evidence) == 0 {
		return ""
	}
	var out strings.Builder
	for _, evidence := range spec.Verification.Evidence {
		from := ""
		if evidence.FromFile {
			from = " It must be published from a file, with publish_artifact's path."
		}
		switch agentID {
		case spec.Verification.Verifier:
			fmt.Fprintf(&out, "\n\nThe verdict rests on evidence: the latest artifact of type %s published by %s. Read it to its end with read_artifact in this process.%s What another process read does not count. Your exit 0 is refused until you have, and a run without it is recorded as unverified, which says nothing about the work. If you cannot read it, use wait and say what you need.", artifact.PublishedPrefix+evidence.Type, evidence.Publisher, from)
		case evidence.Publisher:
			fmt.Fprintf(&out, "\n\nThe verdict rests on what you publish: publish your record as publish_artifact type %s.%s", evidence.Type, from)
		}
	}
	return out.String()
}
