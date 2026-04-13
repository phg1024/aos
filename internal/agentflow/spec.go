package agentflow

import (
	"errors"

	"aos/internal/capability"
)

var ErrWorkflowSpecInvalid = errors.New("invalid workflow spec")

type WorkflowSpec struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	EntryAgent  string      `json:"entry_agent"`
	SkillRoots  []string    `json:"skill_roots,omitempty"`
	Agents      []AgentSpec `json:"agents"`
}

type AgentSpec struct {
	ID            string             `json:"id"`
	SystemPrompt  string             `json:"system_prompt"`
	AllowedAgents []string           `json:"allowed_agents,omitempty"`
	Skills        []string           `json:"skills,omitempty"`
	MaxSteps      int                `json:"max_steps,omitempty"`
	Mounts        []capability.Mount `json:"mounts,omitempty"`
	Capabilities  capability.Set     `json:"capabilities"`
	Metadata      map[string]string  `json:"metadata,omitempty"`
}

func (s WorkflowSpec) Validate() error {
	if s.Name == "" || s.EntryAgent == "" || len(s.Agents) == 0 {
		return ErrWorkflowSpecInvalid
	}
	seen := map[string]bool{}
	hasEntry := false
	for _, agent := range s.Agents {
		if agent.ID == "" || agent.SystemPrompt == "" || seen[agent.ID] {
			return ErrWorkflowSpecInvalid
		}
		seen[agent.ID] = true
		if agent.ID == s.EntryAgent {
			hasEntry = true
		}
	}
	if !hasEntry {
		return ErrWorkflowSpecInvalid
	}
	for _, agent := range s.Agents {
		for _, delegate := range agent.AllowedAgents {
			if !seen[delegate] {
				return ErrWorkflowSpecInvalid
			}
		}
	}
	return nil
}

func (s WorkflowSpec) Agent(id string) (AgentSpec, bool) {
	for _, agent := range s.Agents {
		if agent.ID == id {
			return agent, true
		}
	}
	return AgentSpec{}, false
}
