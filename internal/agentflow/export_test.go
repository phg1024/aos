package agentflow

import (
	"aos/internal/runtime"
	"aos/internal/workflow"
)

// Judge exposes judge, so a test can reach the verdict again from a run's records.
func (e *Engine) Judge(spec WorkflowSpec, policy workflow.VerificationPolicy, rootPID, entryPID string) (Verdict, error) {
	return e.judge(spec, policy, rootPID, entryPID)
}

// EvidenceChecks exposes evidenceChecks, for records a test builds by hand.
func (e *Engine) EvidenceChecks(spec WorkflowSpec, verifierPID string) ([]EvidenceCheck, error) {
	return e.evidenceChecks(spec, verifierPID)
}

// ParseReply exposes parseGenerateResult, so a test can script what a model wrote.
func ParseReply(raw string) (runtime.GenerateResult, bool) { return parseGenerateResult(raw, true) }
