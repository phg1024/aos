package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"aos/internal/replay"
)

// replayRun re-executes a recorded single-agent run against its record:
// recorded replies, recorded tool results, pinned clock. It exits 0 when
// the replay matches, 1 at a divergence, and 2 when the run cannot be
// replayed.
func replayRun(args []string) {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	keep := fs.String("keep", "", "keep the replay's own state in this directory")
	unpinned := fs.Bool("unpinned-clock", false, "leave the budget clock on the wall clock (shows what pinning prevents)")
	positional := parseInterspersed(fs, args)
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: aos replay [--json] [--keep DIR] [--unpinned-clock] <state-dir>")
		os.Exit(2)
	}
	report, err := replay.Run(context.Background(), replay.Options{StateDir: positional[0], WorkDir: *keep, Keep: *keep != "", UnpinnedClock: *unpinned})
	if err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		os.Exit(2)
	}
	build := buildRecord()
	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(struct {
			replay.Report
			ReplayBuild map[string]string `json:"replay_build"`
			Identical   bool              `json:"identical"`
		}{report, build, report.Identical()})
	} else {
		writeReplayReport(os.Stdout, report, build)
	}
	if !report.Identical() {
		os.Exit(1)
	}
}

func writeReplayReport(w io.Writer, report replay.Report, build map[string]string) {
	fmt.Fprintf(w, "replay of %s (workflow %s)\n", report.StateDir, report.Workflow)
	fmt.Fprintf(w, "  calls: %d recorded, %d replayed with identical requests\n", report.Calls, report.Matched)
	fmt.Fprintf(w, "  tool results from the record: %d\n", report.Tools)
	if report.InFlight {
		fmt.Fprintln(w, "  the record ends with a call in flight (the run was killed); compared up to there")
	}
	if report.LegacyRunStarted {
		fmt.Fprintln(w, "  run.started predates recording settings as they take effect; compared by their effect")
	}
	if report.LegacyToolIntents {
		fmt.Fprintln(w, "  the record predates tool.started intents; replayed without them")
	}
	if report.LegacyLeases {
		fmt.Fprintln(w, "  the record predates lease fencing; replayed without it")
	}
	if report.LegacyPeerFraming {
		fmt.Fprintln(w, "  the record predates peer framing; rendered without it")
	}
	if report.LegacyChainDelivery {
		fmt.Fprintln(w, "  the record predates chain-owned delivery; replayed without its events")
	}
	if report.LegacySenderOutcomes {
		fmt.Fprintln(w, "  the record predates sender outcomes; replayed without them")
	}
	if report.LegacyPublishSource {
		fmt.Fprintln(w, "  the record predates publish_artifact's path; replayed with typed bodies only")
	}
	if report.LegacyDurationArgs {
		fmt.Fprintln(w, "  the record predates how tools read a duration; replayed with its prompt and lease ttls")
	}
	if report.LegacyDelegateHandles {
		fmt.Fprintln(w, "  the record predates handles at a delegation; replayed with its prompt")
	}
	if report.LegacyBusBounds {
		fmt.Fprintln(w, "  the record predates the bus's bounds and handles; replayed without them")
	}
	if report.Identical() {
		fmt.Fprintf(w, "  kernel: %d events and the process table identical\n", report.Events)
		fmt.Fprintf(w, "  artifacts: %d identical (checkpoint usage seconds not compared)\n", report.Artifacts)
		fmt.Fprintln(w, "  result: identical")
	} else {
		d := report.Divergence
		where := ""
		if d.Call > 0 {
			where = fmt.Sprintf(" at call %d (step %d, %s)", d.Call, d.Step, d.PID)
		} else if d.PID != "" {
			where = " in " + d.PID
		}
		fmt.Fprintf(w, "  first divergence: %s%s\n    %s\n", d.Kind, where, d.Detail)
		if d.Recorded != "" {
			fmt.Fprintf(w, "    recorded: %q\n", d.Recorded)
		}
		if d.Replayed != "" {
			fmt.Fprintf(w, "    replayed: %q\n", d.Replayed)
		}
	}
	if recorded, replaying := report.RecordedBuild["build_sha256"], build["build_sha256"]; recorded != "" && recorded != replaying {
		fmt.Fprintf(w, "  note: recorded by another build (%s), replayed by %s\n", describeBuild(report.RecordedBuild), describeBuild(build))
	}
	if report.ReplayDir != "" {
		fmt.Fprintf(w, "  replay state: %s\n", report.ReplayDir)
	}
}

func describeBuild(build map[string]string) string {
	out := build["build_version"]
	if revision := build["build_revision"]; revision != "" {
		out += " " + revision[:min(12, len(revision))]
	}
	if sum := build["build_sha256"]; sum != "" {
		out += " sha256:" + sum[:12]
	}
	return out
}
