package skills_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/skills"
)

func TestRegistryLoadsNamedSkillFromRoot(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "writer")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	path := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(path, []byte("# Writer\nUse concise prose."), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}

	registry := skills.NewRegistry([]string{root})
	doc, err := registry.Load("writer")
	if err != nil {
		t.Fatalf("load skill: %v", err)
	}
	if doc.Name != "writer" || doc.Path != path {
		t.Fatalf("unexpected document %#v", doc)
	}
	if !strings.Contains(doc.Content, "Use concise prose") {
		t.Fatalf("unexpected content %q", doc.Content)
	}
}

func TestRegistryLoadsSkillByPath(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "research")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	path := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(path, []byte("# Research\nGather sources."), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}

	registry := skills.NewRegistry(nil)
	doc, err := registry.Load(skillDir)
	if err != nil {
		t.Fatalf("load skill by path: %v", err)
	}
	if doc.Name != "research" {
		t.Fatalf("unexpected name %q", doc.Name)
	}
}
