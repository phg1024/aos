package runtime

import (
	"errors"
	"reflect"
	"testing"
)

// Wherever a reply put a delegation's artifacts, they end up where the action reads
// them, and two places that differ, or an action that attaches nothing, are refused.
func TestPlaceArtifacts(t *testing.T) {
	delegate := func(inner, outer string) GenerateAction {
		return GenerateAction{Type: "delegate", Delegate: &DelegateCall{AgentID: "v", Goal: "g", Artifacts: inner}, Artifacts: outer}
	}
	tool := func(name string, args map[string]string, beside, outer string) GenerateAction {
		return GenerateAction{Type: "tool", ToolCall: &ToolCall{Name: name, Args: args, Artifacts: beside}, Artifacts: outer}
	}
	for _, c := range []struct {
		name    string
		action  GenerateAction
		want    GenerateAction
		refused bool
	}{
		{"inside", delegate("art-1", ""), delegate("art-1", ""), false},
		{"inside, as written", delegate("art-1, art-2", ""), delegate("art-1, art-2", ""), false},
		{"at the action level", delegate("", "art-1"), delegate("art-1", ""), false},
		{"both, the same", delegate("art-1,art-2", "art-2, art-1"), delegate("art-1,art-2", ""), false},
		{"both, different", delegate("art-1", "art-2"), delegate("art-1", "art-2"), true},
		{"neither", delegate("", ""), delegate("", ""), false},
		{"spawn_agent at the action level", tool("spawn_agent", map[string]string{"agent_id": "v"}, "", "art-1"),
			tool("spawn_agent", map[string]string{"agent_id": "v", "artifacts": "art-1"}, "", ""), false},
		{"spawn_agent beside args", tool("spawn_agent", nil, "art-1", ""), tool("spawn_agent", map[string]string{"artifacts": "art-1"}, "", ""), false},
		{"message_agent at the action level", tool("message_agent", map[string]string{"to": "p"}, "", "art-1"),
			tool("message_agent", map[string]string{"to": "p", "artifacts": "art-1"}, "", ""), false},
		{"message_agent, three places that differ", tool("message_agent", map[string]string{"artifacts": "art-1"}, "art-1", "art-3"),
			tool("message_agent", map[string]string{"artifacts": "art-1"}, "art-1", "art-3"), true},
		{"another tool", tool("read_artifact", map[string]string{"id": "art-1"}, "", "art-1"), tool("read_artifact", map[string]string{"id": "art-1"}, "", "art-1"), true},
		{"a send", GenerateAction{Type: "send", Send: &SendCall{To: "p", Body: "b", Artifacts: "art-1"}}, GenerateAction{Type: "send", Send: &SendCall{To: "p", Body: "b", Artifacts: "art-1"}}, true},
		{"an exit", GenerateAction{Type: "exit", Artifacts: "art-1"}, GenerateAction{Type: "exit", Artifacts: "art-1"}, true},
		{"an exit, empty", GenerateAction{Type: "exit", Artifacts: " , "}, GenerateAction{Type: "exit", Artifacts: " , "}, false},
		{"a malformed delegate", GenerateAction{Type: "delegate", Artifacts: "art-1"}, GenerateAction{Type: "delegate", Artifacts: "art-1"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			action := c.action
			err := placeArtifacts(&action)
			if refused := errors.Is(err, ErrMisplacedArtifacts); refused != c.refused || (err != nil && !refused) {
				t.Fatalf("err = %v, want refused %t", err, c.refused)
			}
			if !reflect.DeepEqual(action, c.want) {
				t.Fatalf("got %+v %+v\nwant %+v %+v", action, action.ToolCall, c.want, c.want.ToolCall)
			}
		})
	}
}
