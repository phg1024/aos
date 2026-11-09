package agentflow

import (
	"fmt"
	"maps"
	"strconv"
	"time"

	"aos/internal/capability"
	"aos/internal/runtime"
)

// RunStartedEvent is the kernel event a workflow run records on its root
// process before the entry agent starts: what the run ran under, so the
// record states it and a replay can set it up again. Its data holds the
// run's budget, the engine's compaction and retry settings, the run_spec
// artifact holding the workflow and request, and whatever the caller puts
// in Engine.RunRecord (the build and the backend).
const RunStartedEvent = "run.started"

// LeaseFencingField is the run.started field that says file writes were
// fenced by leases, with the value LeaseFencingFileWrites.
const (
	LeaseFencingField      = "lease_fencing"
	LeaseFencingFileWrites = "file_writes"
)

// PeerFramingField is the run.started field that says peer messages and
// runtime-written turns reach the model framed, with the value
// PeerFramingV2. Records without it used the older rendering.
const (
	PeerFramingField = "peer_framing"
	PeerFramingV2    = "v2"
)

// ChainDeliveryField is the run.started field that says mail and
// subscriptions belong to the process chain, with the value ChainDeliveryV1:
// the run's record holds process.closed, ipc.subscribed and ipc.unsubscribed.
// Records without it do not.
const (
	ChainDeliveryField = "chain_delivery"
	ChainDeliveryV1    = "v1"
)

// SenderOutcomesField is the run.started field that says mail no model was
// shown is tracked and dead-lettered, with the value SenderOutcomesV1. Records
// without it acknowledged every recorded message, and hold no dead letters.
const (
	SenderOutcomesField = "sender_outcomes"
	SenderOutcomesV1    = "v1"
)

// BusBoundsField is the run.started field that says the bus held mail to its cap
// and bound, and that handles and the gated list_artifacts were in force, with
// the value BusBoundsV1. Records without it took any size into any mailbox.
const (
	BusBoundsField = "bus_bounds"
	BusBoundsV1    = "v1"
)

// RedelegationCloseField is the run.started field that says a delegator's new
// delegation of an agent closes the chain of its retryable exit, with the value
// RedelegationCloseV1. Records without it left that chain open.
const (
	RedelegationCloseField = "redelegation_close"
	RedelegationCloseV1    = "v1"
	// PublishSourceField is the run.started field that says publish_artifact takes
	// a path and read_artifact says where a body came from: PublishSourceV1. Records
	// without it ran with typed bodies only, and readers were told nothing.
	PublishSourceField = "publish_source"
	PublishSourceV1    = "v1"
	// DurationArgsField is the run.started field that says tools read a duration arg
	// through tools.DurationArg and the prompt describes it: DurationArgsV1. Records
	// without it fell back to the default for a bare number or an unreadable value.
	DurationArgsField = "duration_args"
	DurationArgsV1    = "v1"
	// DelegateHandlesField is the run.started field that says the prompt offers handles
	// at a delegation: DelegateHandlesV1. Records without it never offered them.
	DelegateHandlesField = "delegate_handles"
	DelegateHandlesV1    = "v1"
	// DelegateAttachField is the run.started field that says a reply's artifacts were
	// read wherever it put them, misplaced ones refused, and a delegator told what its
	// delegation attached: DelegateAttachV1.
	DelegateAttachField = "delegate_attach"
	DelegateAttachV1    = "v1"
	// ReplyChecksField is the run.started field that says replies were read with
	// the reply checks: a <think> block stripped before the ladder, a reply holding
	// two different actions sent to the repair call, and an incomplete flat-shape
	// action refused (ReplyChecksV1). Records without it read replies as before.
	ReplyChecksField = "reply_checks"
	ReplyChecksV1    = "v1"
	// ReplyCapField is the run.started field that says each call's reply was capped
	// at the token budget left less the prompt, and a call with no room refused
	// (ReplyCapV1). Records without it capped the reply at the budget left.
	ReplyCapField = "reply_cap"
	ReplyCapV1    = "v1"
	// SpecContractField is the run.started field that says the spec was checked
	// against the spec contract (WorkflowSpec.Validate, and a cycle's budget at
	// Engine.Run): SpecContractV1. Records without it were checked with the
	// rules before it, and a replay checks them so.
	SpecContractField = "spec_contract"
	SpecContractV1    = "v1"
	// SpecContractV2 adds the verification rules: trusted paths, gate checks,
	// and no unconfined_run or work scope of / under a required gate.
	SpecContractV2 = "v2"
	// VerdictRulesField is the run.started field that says a verification was
	// judged under the verdict rules (VerdictRulesV1): a pass bound to a hash
	// of the work, the gate's own checks, the verifier's task from the spec,
	// a failed best_effort verdict exiting non-zero, and a standing failure
	// not retryable. Records without it were judged as before.
	VerdictRulesField = "verdict_rules"
	VerdictRulesV1    = "v1"
	// ExitOriginField is the run.started field that says the exits the runtime
	// and the engine record name their origin (ExitOriginV1). Records without
	// it left those origins empty.
	ExitOriginField = "exit_origin"
	ExitOriginV1    = "v1"
	// PublishProvenanceField is the run.started field that says a publication
	// from a file records the run that produced it (PublishProvenanceV1).
	PublishProvenanceField = "publish_provenance"
	PublishProvenanceV1    = "v1"
)

// LeaseClockField is the run.started field that says lease expiries come
// from the kernel's clock, with the value LeaseClockKernel, so a replay
// reproduces them. Records without it took expiries from the wall clock.
const (
	LeaseClockField  = "lease_clock"
	LeaseClockKernel = "kernel"
)

// RunSpecArtifact is the artifact type holding a run's workflow spec, as
// the engine ran it, and its request.
const RunSpecArtifact = "run_spec"

// RunSpec is a run_spec artifact's payload.
type RunSpec struct {
	Spec    WorkflowSpec `json:"spec"`
	Request RunRequest   `json:"request"`
}

// runStartedData is the run.started event's data. Settings are recorded
// as they take effect, defaults filled in, so the record can be read
// without knowing the code's defaults; *_source says whether a setting was
// configured or is the default, so a replay can set it up the same way.
// A limit of 0 means none: budget_tokens and budget_seconds unlimited,
// compact_max_messages and compact_keep no limit.
func (e *Engine) runStartedData(spec WorkflowSpec, specID, specSHA string) map[string]string {
	compaction, retry := e.Compaction.Effective(), e.BackendRetry.Effective()
	data := map[string]string{
		"run_spec":             specID,
		"run_spec_sha256":      specSHA,
		"budget_tokens":        strconv.Itoa(e.Budget.Tokens),
		"budget_seconds":       strconv.Itoa(e.Budget.Seconds),
		"budget_strict_time":   strconv.FormatBool(e.Budget.StrictTime),
		"budget_time":          timeBudgetKind(e.Budget),
		"compaction_source":    source(e.Compaction == (runtime.CompactionPolicy{})),
		"compact_max_messages": strconv.Itoa(compaction.MaxMessages),
		"compact_max_chars":    strconv.Itoa(compaction.MaxChars),
		"compact_keep":         strconv.Itoa(compaction.KeepMessages),
		"compact_target_chars": strconv.Itoa(compaction.TargetChars),
		"compact_handoff":      strconv.FormatBool(compaction.Handoff),
		"retry_source":         source(e.BackendRetry == (runtime.BackendRetryPolicy{})),
		"retry_attempts":       strconv.Itoa(retry.Attempts),
		"retry_base":           retry.Base.String(),
		"retry_max":            retry.Max.String(),
	}
	for _, agent := range spec.Agents {
		data["max_steps_"+agent.ID] = strconv.Itoa(maxSteps(agent))
	}
	if !e.NoLeaseClock {
		data[LeaseClockField] = LeaseClockKernel
	}
	if !e.NoLeaseFencing {
		// Marks a run whose file writes are fenced by leases, so a replay
		// knows to fence them too; records made before have no such field.
		data[LeaseFencingField] = LeaseFencingFileWrites
	}
	if !e.NoPeerFraming {
		data[PeerFramingField] = PeerFramingV2
	}
	if !e.NoChainDelivery {
		data[ChainDeliveryField] = ChainDeliveryV1
	}
	if !e.NoSenderOutcomes {
		data[SenderOutcomesField] = SenderOutcomesV1
	}
	if !e.NoBusBounds {
		data[BusBoundsField] = BusBoundsV1
	}
	if !e.NoPublishSource {
		data[PublishSourceField] = PublishSourceV1
	}
	if !e.NoDurationArgs {
		data[DurationArgsField] = DurationArgsV1
	}
	if !e.NoDelegateHandles {
		data[DelegateHandlesField] = DelegateHandlesV1
	}
	if !e.NoDelegateAttach {
		data[DelegateAttachField] = DelegateAttachV1
	}
	if !e.NoRedelegationClose {
		data[RedelegationCloseField] = RedelegationCloseV1
	}
	if !e.NoReplyChecks {
		data[ReplyChecksField] = ReplyChecksV1
	}
	if !e.NoReplyCap {
		data[ReplyCapField] = ReplyCapV1
	}
	switch {
	case e.NoSpecContract:
	case e.SpecContractV1:
		data[SpecContractField] = SpecContractV1
	default:
		data[SpecContractField] = SpecContractV2
	}
	if !e.NoVerdictRules {
		data[VerdictRulesField] = VerdictRulesV1
	}
	if !e.NoExitOrigin {
		data[ExitOriginField] = ExitOriginV1
	}
	if !e.NoPublishProvenance {
		data[PublishProvenanceField] = PublishProvenanceV1
	}
	for name, value := range e.RunRecord {
		if _, own := data[name]; !own {
			data[name] = value
		}
	}
	return data
}

// timeBudgetKind is how a time budget binds: none, advisory or strict.
func timeBudgetKind(budget capability.Budget) string {
	switch {
	case budget.Seconds <= 0:
		return "none"
	case budget.StrictTime:
		return "strict"
	}
	return "advisory"
}

func source(isDefault bool) string {
	if isDefault {
		return "default"
	}
	return "configured"
}

// RunSettings are the engine settings a run.started event records.
type RunSettings struct {
	Budget       capability.Budget
	Compaction   runtime.CompactionPolicy
	BackendRetry runtime.BackendRetryPolicy
	// Record is the event's data, for the fields the caller added.
	Record map[string]string
}

// ParseRunStarted reads the settings back from a run.started event's data.
func ParseRunStarted(data map[string]string) (RunSettings, error) {
	var settings RunSettings
	var err error
	integer := func(name string) int {
		if err != nil {
			return 0
		}
		var value int
		if value, err = strconv.Atoi(data[name]); err != nil {
			err = fmt.Errorf("run.started %s: %w", name, err)
		}
		return value
	}
	boolean := func(name string) bool {
		if err != nil {
			return false
		}
		var value bool
		if value, err = strconv.ParseBool(data[name]); err != nil {
			err = fmt.Errorf("run.started %s: %w", name, err)
		}
		return value
	}
	duration := func(name string) time.Duration {
		if err != nil {
			return 0
		}
		var value time.Duration
		if value, err = time.ParseDuration(data[name]); err != nil {
			err = fmt.Errorf("run.started %s: %w", name, err)
		}
		return value
	}
	settings.Budget = capability.Budget{Tokens: integer("budget_tokens"), Seconds: integer("budget_seconds"), StrictTime: boolean("budget_strict_time")}
	settings.Compaction = runtime.CompactionPolicy{
		MaxMessages:  integer("compact_max_messages"),
		MaxChars:     integer("compact_max_chars"),
		KeepMessages: integer("compact_keep"),
		TargetChars:  integer("compact_target_chars"),
		Handoff:      boolean("compact_handoff"),
	}
	settings.BackendRetry = runtime.BackendRetryPolicy{Attempts: integer("retry_attempts"), Base: duration("retry_base"), Max: duration("retry_max")}
	// A default setting is set up as the default again, which behaves the
	// same and records the same.
	if data["compaction_source"] == "default" {
		settings.Compaction = runtime.CompactionPolicy{}
	}
	if data["retry_source"] == "default" {
		settings.BackendRetry = runtime.BackendRetryPolicy{}
	}
	settings.Record = maps.Clone(data)
	return settings, err
}
