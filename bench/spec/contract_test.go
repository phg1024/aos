//go:build workflowspec

package spec_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"aos/internal/agentflow"
)

// S6c and S10c read the S6 typos and the checked-in workflows through
// agentflow.DecodeWorkflowSpec, the strict decoder aos run uses since the
// spec contract. It does not exist at v0.1.7, so this file is left out of a
// run there; spec_test.go's probes decode with json.Unmarshal, as v0.1.7's
// aos run did, so those rows read the same either way.
func TestS6cStrictDecoding(t *testing.T) {
	specs := []struct{ name, raw string }{
		{`"verifcation"`, `{"name":"s6","entry_agent":"a","verifcation":{"policy":"required","verifier":"a"},"agents":[{"id":"a","system_prompt":"A."}]}`},
		{`"max_step"`, `{"name":"s6","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","max_step":2}]}`},
		{`"capabilites"`, `{"name":"s6","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","capabilites":{"tools":["write_file"]}}]}`},
		{`"allowed_agent"`, `{"name":"s6","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","allowed_agent":["a"]}]}`},
		{`per-agent "model"`, `{"name":"s6","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","model":"claude-sonnet-5-5","profile":"claude"}]}`},
		{`"max_steps": 0`, `{"name":"s4","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","max_steps":0}]}`},
	}
	for _, s := range specs {
		_, err := agentflow.DecodeWorkflowSpec([]byte(s.raw))
		finding(t, "S6c %-18s DecodeWorkflowSpec: %s", s.name+":", errText(err))
	}
}

func TestS10cCheckedInWorkflowsStrictly(t *testing.T) {
	paths, _ := filepath.Glob("../../workflows/*.json")
	sort.Strings(paths)
	for _, path := range paths {
		raw, _ := os.ReadFile(path)
		spec, err := agentflow.DecodeWorkflowSpec(raw)
		if err == nil {
			err = spec.Validate()
		}
		finding(t, "S10c %-40s DecodeWorkflowSpec and Validate: %s", filepath.Base(path), errText(err))
	}
}
