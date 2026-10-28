package artifact_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"aos/internal/artifact"
)

func newStore(t *testing.T) (*artifact.Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := artifact.New(root)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store, root
}

func TestTamperedPayloadIsDetected(t *testing.T) {
	store, _ := newStore(t)
	record, err := store.CreateJSON("checkpoint", "pid-1", map[string]string{"verdict": "FAIL"}, nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	info, err := os.Stat(record.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o444 || len(record.SHA256) != 64 {
		t.Fatalf("payload should be read-only and hashed, mode %v sha %q", info.Mode().Perm(), record.SHA256)
	}
	if err := os.Chmod(record.Path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.WriteFile(record.Path, []byte(`{"verdict":"PASS"}`), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	var payload map[string]string
	if err := store.ReadJSON(record.ID, &payload); !errors.Is(err, artifact.ErrArtifactTampered) {
		t.Fatalf("expected ErrArtifactTampered, got %v (payload %v)", err, payload)
	}
}

// A crash mid-write leaves at most a stray temp file, never a torn index,
// and a stale counter cannot reuse an ID.
func TestCrashLeftoversDoNotBreakTheStore(t *testing.T) {
	store, root := newStore(t)
	first, err := store.CreateJSON("note", "pid-1", "a", nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".index.json.tmp-crashed"), []byte(`{"art-0000`), 0o644); err != nil {
		t.Fatalf("leftover: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(`{"next_artifact_id":1}`), 0o644); err != nil {
		t.Fatalf("stale counter: %v", err)
	}
	reopened, err := artifact.New(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	second, err := reopened.CreateJSON("note", "pid-1", "b", nil, nil)
	if err != nil {
		t.Fatalf("create after crash: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("a stale counter reused %s", first.ID)
	}
	var value string
	if err := reopened.ReadJSON(first.ID, &value); err != nil || value != "a" {
		t.Fatalf("earlier artifact unreadable after crash leftovers: %q %v", value, err)
	}
}

func TestLineageIsChecked(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.CreateJSON("note", "pid-1", "x", []string{"art-999999"}, nil); !errors.Is(err, artifact.ErrUnknownParent) {
		t.Fatalf("expected ErrUnknownParent, got %v", err)
	}
	store.BindProducers(func(pid string) error {
		if pid != "pid-1" {
			return errors.New("no such process")
		}
		return nil
	})
	if _, err := store.CreateJSON("note", "pid-404", "x", nil, nil); !errors.Is(err, artifact.ErrUnknownProducer) {
		t.Fatalf("expected ErrUnknownProducer, got %v", err)
	}
	parent, err := store.CreateJSON("note", "pid-1", "x", nil, nil)
	if err != nil {
		t.Fatalf("known producer: %v", err)
	}
	if _, err := store.CreateJSON("note", "pid-1", "y", []string{parent.ID}, nil); err != nil {
		t.Fatalf("known parent: %v", err)
	}
}

// Stores written before hashing load and read without verification.
func TestLegacyStoreLoads(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	payload := filepath.Join(root, "payloads", "art-000001.json")
	if err := os.MkdirAll(filepath.Dir(payload), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	files := map[string]string{
		payload:                           `{"body":"old"}`,
		filepath.Join(root, "state.json"): `{"next_artifact_id":2}`,
		filepath.Join(root, "index.json"): `{"art-000001":{"id":"art-000001","type":"note","producer_pid":"pid-1","path":"` + payload + `","created_at":"2026-10-01T00:00:00Z"}}`,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	store, err := artifact.New(root)
	if err != nil {
		t.Fatalf("legacy store should open: %v", err)
	}
	var body map[string]string
	if err := store.ReadJSON("art-000001", &body); err != nil || body["body"] != "old" {
		t.Fatalf("legacy payload: %v %v", body, err)
	}
	next, err := store.CreateJSON("note", "pid-1", "new", []string{"art-000001"}, nil)
	if err != nil || next.ID != "art-000002" || next.SHA256 == "" {
		t.Fatalf("new artifact after legacy entries: %+v %v", next, err)
	}
}

func TestIsReserved(t *testing.T) {
	for _, reserved := range []string{"checkpoint", "verification", "memory_segment", "agent_run"} {
		if !artifact.IsReserved(reserved) {
			t.Fatalf("%s should be reserved", reserved)
		}
	}
	if artifact.IsReserved("note") || artifact.IsReserved(artifact.PublishedPrefix+"checkpoint") {
		t.Fatal("published types are not reserved")
	}
}
