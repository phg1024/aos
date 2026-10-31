// Command headercheck renders, without a model call, how much of each
// agent index the header of each probe checkpoint shows with a1 on, for
// gates G2 and G3 of the A1 header re-test (../PLAN.md). It reads the
// checkpoints from the run's state directory, sets each draw's indexes on
// their segment references as memprobe does, and asks the prompt backend
// for its header sizes. Nothing is written to the state directory.
//
// Usage, from the repository root:
//
//	go run ./reports/memory-probe-recall-header/headercheck <state-dir> <indexes-d1.json> ...
//
// It prints JSON: for each indexes file, checkpoint and segment, the lines
// shown and hidden (the header shows an index's first lines), and the
// header's budget and agent-index bytes per checkpoint.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"aos/internal/agentflow"
	"aos/internal/memprobe"
	"aos/internal/runtime"
)

var checkpoints = []string{"art-000648", "art-001768", "art-003135"}

type segmentShown struct {
	Lines  int `json:"lines"`
	Shown  int `json:"shown"`
	Hidden int `json:"hidden"`
}

type header struct {
	Budget     int                     `json:"budget"`
	Bytes      int                     `json:"agent_index_bytes"`
	Segments   int                     `json:"segments_showing"`
	ByS        map[string]segmentShown `json:"by_segment"`
	NoIndexFor []string                `json:"no_index_for,omitempty"`
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: headercheck <state-dir> <indexes.json> ...")
		os.Exit(2)
	}
	policy, err := memprobe.RecallPolicy([]string{"a1"})
	if err != nil {
		fail(err)
	}
	backend := agentflow.PromptBackend{Recall: policy}
	out := map[string]map[string]header{}
	for _, path := range os.Args[2:] {
		var indexes map[string][]runtime.IndexLine
		if err := readJSON(path, &indexes); err != nil {
			fail(err)
		}
		byCheckpoint := map[string]header{}
		for _, checkpoint := range checkpoints {
			var session runtime.Session
			if err := readJSON(filepath.Join(os.Args[1], "artifacts", "payloads", checkpoint+".json"), &session); err != nil {
				fail(err)
			}
			h := header{ByS: map[string]segmentShown{}}
			for i, ref := range session.MemoryRefs {
				lines, ok := indexes[ref.ArtifactID]
				if !ok || len(lines) == 0 {
					h.NoIndexFor = append(h.NoIndexFor, ref.ArtifactID)
					continue
				}
				for _, line := range lines {
					if line.From < ref.FromSeq || line.To > ref.ToSeq || line.From > line.To {
						fail(fmt.Errorf("%s: index line %q lies outside segment %s", path, line, ref.ArtifactID))
					}
				}
				if size := len(runtime.RenderIndex(lines)); size > runtime.MaxSegmentIndexBytes {
					fail(fmt.Errorf("%s: index of %s is %d bytes, over %d", path, ref.ArtifactID, size, runtime.MaxSegmentIndexBytes))
				}
				session.MemoryRefs[i].Index = lines
			}
			sizes := backend.HeaderSizes(runtime.GenerateRequest{MemoryRefs: session.MemoryRefs})
			h.Budget, h.Bytes, h.Segments = sizes["agent_index_budget"], sizes["agent_index_bytes"], sizes["agent_index_segments"]
			for _, ref := range session.MemoryRefs {
				if len(ref.Index) > 0 {
					h.ByS[ref.ArtifactID] = segmentShown{Lines: len(ref.Index),
						Shown: sizes["agent_index_shown."+ref.ArtifactID], Hidden: sizes["agent_index_hidden."+ref.ArtifactID]}
				}
			}
			byCheckpoint[checkpoint] = h
		}
		out[filepath.Base(path)] = byCheckpoint
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", " ")
	if err := encoder.Encode(out); err != nil {
		fail(err)
	}
}

func readJSON(path string, v any) error {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, v)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "headercheck:", err)
	os.Exit(1)
}
