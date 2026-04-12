package artifact_test

import (
	"path/filepath"
	"testing"

	"aos/internal/artifact"
)

func TestArtifactStoreCreatesAndReadsJSON(t *testing.T) {
	store, err := artifact.New(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	created, err := store.CreateJSON("plan", "pid-1", map[string]any{
		"steps": []string{"a", "b"},
	}, []string{"art-root"}, map[string]string{"role": "planner"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	meta, err := store.Metadata(created.ID)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if len(meta.ParentIDs) != 1 || meta.ParentIDs[0] != "art-root" {
		t.Fatalf("unexpected parents %#v", meta.ParentIDs)
	}

	var payload map[string]any
	if err := store.ReadJSON(created.ID, &payload); err != nil {
		t.Fatalf("read: %v", err)
	}
	if payload["steps"] == nil {
		t.Fatalf("expected payload to round-trip, got %#v", payload)
	}
}
