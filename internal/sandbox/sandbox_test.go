package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeCard(t *testing.T, root, pci, uuid, minor string) {
	t.Helper()
	dir := filepath.Join(root, pci)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	info := "Model: \t\t A card\nIRQ:   \t\t 42\nGPU UUID: \t " + uuid + "\nVideo BIOS: \t 94.02\nBus Type: \t PCIe\nDevice Minor: \t " + minor + "\nBlacklisted: \t No\n"
	if err := os.WriteFile(filepath.Join(dir, "information"), []byte(info), 0o444); err != nil {
		t.Fatal(err)
	}
}

// One card named gets its own node and the shared two, and the other card's stays out.
func TestResolveGPUsGrantsTheNamedCardOnly(t *testing.T) {
	root := t.TempDir()
	writeCard(t, root, "0000:01:00.0", "GPU-aaaa", "0")
	writeCard(t, root, "0000:02:00.0", "GPU-bbbb", "1")
	got, err := resolveGPUs(root, []string{"GPU-bbbb"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia1"}) {
		t.Fatalf("devices = %v", got)
	}
	both, err := resolveGPUs(root, []string{"GPU-aaaa", "GPU-bbbb"})
	if err != nil || !slices.Equal(both, []string{"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia0", "/dev/nvidia1"}) {
		t.Fatalf("both = %v, %v", both, err)
	}
}

// A UUID not here refuses, whatever else is here: a run never lands on another card.
func TestResolveGPUsRefusesACardNotHere(t *testing.T) {
	root := t.TempDir()
	writeCard(t, root, "0000:01:00.0", "GPU-aaaa", "0")
	_, err := resolveGPUs(root, []string{"GPU-aaaa", "GPU-cccc"})
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "GPU-cccc") {
		t.Fatalf("err = %v", err)
	}
	if _, err := resolveGPUs(filepath.Join(root, "no-driver"), []string{"GPU-aaaa"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no driver: %v", err)
	}
}

func TestEnvironmentNamesVisibleGPUsOnlyWhenGranted(t *testing.T) {
	plain := Policy{Temp: "/tmp/x"}.Environment([]string{"CUDA_VISIBLE_DEVICES=0", "HOME=/h"})
	if !slices.Contains(plain, "CUDA_VISIBLE_DEVICES=0") {
		t.Fatalf("with no grant the caller's value should stand: %v", plain)
	}
	granted := Policy{Temp: "/tmp/x", VisibleGPUs: []string{"GPU-a", "GPU-b"}}.Environment([]string{"CUDA_VISIBLE_DEVICES=0"})
	if !slices.Contains(granted, "CUDA_VISIBLE_DEVICES=GPU-a,GPU-b") || slices.Contains(granted, "CUDA_VISIBLE_DEVICES=0") {
		t.Fatalf("granted = %v", granted)
	}
}

// No policy can name a key back in: there is no field for it, and the temporary and
// cache overrides are fixed.
func TestEnvironmentRemovesModelKeys(t *testing.T) {
	got := Policy{Temp: "/t", VisibleGPUs: []string{"GPU-a"}}.Environment([]string{
		"AOS_LLM_API_KEY=a", "AOS_VLLM_API_KEY=b", "AOS_KEY_ANYTHING=c", "PATH=/bin", "AOS_LLM_MODEL=m",
	})
	joined := strings.Join(got, "\n")
	for _, name := range []string{"AOS_LLM_API_KEY", "AOS_VLLM_API_KEY", "AOS_KEY_ANYTHING"} {
		if strings.Contains(joined, name) {
			t.Errorf("%s stayed:\n%s", name, joined)
		}
	}
	for _, kept := range []string{"PATH=/bin", "AOS_LLM_MODEL=m", "TMPDIR=/t", "CUDA_VISIBLE_DEVICES=GPU-a"} {
		if !slices.Contains(got, kept) {
			t.Errorf("%s is gone:\n%s", kept, joined)
		}
	}
}
