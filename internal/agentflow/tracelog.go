package agentflow

import (
	"time"

	"aos/internal/runlog"
)

// TraceRecord is one line of the durable trace (runlog.TraceFile).
type TraceRecord struct {
	Seq         int       `json:"seq"`
	Time        time.Time `json:"time"`
	AgentID     string    `json:"agent_id"`
	PID         string    `json:"pid"`
	Kind        string    `json:"kind"`
	Detail      string    `json:"detail"`
	KernelEvent string    `json:"kernel_event,omitempty"`
}

// TraceToLog writes every trace event to log as it is emitted, so the
// trace is on disk before the agent's next step. Write errors are dropped:
// the trace is diagnostic, and the kernel log stays authoritative.
func TraceToLog(log *runlog.Log) func(TraceEvent) {
	return func(event TraceEvent) {
		_, _ = log.Append(func(seq int) any {
			return TraceRecord{Seq: seq, Time: event.Time.UTC(), AgentID: event.AgentID, PID: event.PID, Kind: event.Kind, Detail: event.Detail, KernelEvent: event.KernelEvent}
		})
	}
}

// Tracers fans one trace event out to several sinks, skipping nil ones.
func Tracers(sinks ...func(TraceEvent)) func(TraceEvent) {
	return func(event TraceEvent) {
		for _, sink := range sinks {
			if sink != nil {
				sink(event)
			}
		}
	}
}
